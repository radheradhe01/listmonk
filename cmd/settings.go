package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/gdgvda/cron"
	"github.com/gofrs/uuid/v5"
	"github.com/jmoiron/sqlx/types"
	koanfjson "github.com/knadh/koanf/parsers/json"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"
	"github.com/knadh/listmonk/internal/auth"
	"github.com/knadh/listmonk/internal/manager"
	"github.com/knadh/listmonk/internal/messenger/email"
	"github.com/knadh/listmonk/internal/notifs"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
)

const pwdMask = "•"

type aboutHost struct {
	OS       string `json:"os"`
	Machine  string `json:"arch"`
	Hostname string `json:"hostname"`
}

type aboutSystem struct {
	NumCPU  int    `json:"num_cpu"`
	AllocMB uint64 `json:"memory_alloc_mb"`
	OSMB    uint64 `json:"memory_from_os_mb"`
}

type about struct {
	Version   string         `json:"version"`
	Build     string         `json:"build"`
	GoVersion string         `json:"go_version"`
	GoArch    string         `json:"go_arch"`
	Database  types.JSONText `json:"database"`
	System    aboutSystem    `json:"system"`
	Host      aboutHost      `json:"host"`
}

var (
	reAlphaNum = regexp.MustCompile(`[^a-z0-9\-]`)
)

// GetSettings returns settings from the DB.
func (a *App) GetSettings(c echo.Context) error {
	s, err := a.core.GetSettings()
	if err != nil {
		return err
	}

	// Empty out passwords.
	for i := range s.SMTP {
		s.SMTP[i].Password = strings.Repeat(pwdMask, utf8.RuneCountInString(s.SMTP[i].Password))
	}
	for i := range s.BounceBoxes {
		s.BounceBoxes[i].Password = strings.Repeat(pwdMask, utf8.RuneCountInString(s.BounceBoxes[i].Password))
	}
	for i := range s.Messengers {
		s.Messengers[i].Password = strings.Repeat(pwdMask, utf8.RuneCountInString(s.Messengers[i].Password))
	}

	s.UploadS3AwsSecretAccessKey = strings.Repeat(pwdMask, utf8.RuneCountInString(s.UploadS3AwsSecretAccessKey))
	s.SendgridKey = strings.Repeat(pwdMask, utf8.RuneCountInString(s.SendgridKey))
	s.BouncePostmark.Password = strings.Repeat(pwdMask, utf8.RuneCountInString(s.BouncePostmark.Password))
	s.BounceForwardEmail.Key = strings.Repeat(pwdMask, utf8.RuneCountInString(s.BounceForwardEmail.Key))
	s.SecurityCaptcha.HCaptcha.Secret = strings.Repeat(pwdMask, utf8.RuneCountInString(s.SecurityCaptcha.HCaptcha.Secret))
	s.OIDC.ClientSecret = strings.Repeat(pwdMask, utf8.RuneCountInString(s.OIDC.ClientSecret))

	return c.JSON(http.StatusOK, okResp{s})
}

// UpdateSettings returns settings from the DB.
func (a *App) UpdateSettings(c echo.Context) error {
	// Unmarshal and marshal the fields once to sanitize the settings blob.
	var set models.Settings
	if err := c.Bind(&set); err != nil {
		return err
	}

	// Get the existing settings.
	cur, err := a.core.GetSettings()
	if err != nil {
		return err
	}

	// Validate and sanitize postback Messenger names along with SMTP names
	// (where each SMTP is also considered as a standalone messenger).
	// Duplicates are disallowed and "email" is a reserved name.
	names := map[string]bool{emailMsgr: true}

	// There should be at least one SMTP block that's enabled.
	has := false
	for i, s := range set.SMTP {
		if s.Enabled {
			has = true
		}

		// Sanitize and normalize the SMTP server name.
		name := reAlphaNum.ReplaceAllString(strings.ToLower(strings.TrimSpace(s.Name)), "-")
		if name != "" {
			if !strings.HasPrefix(name, "email-") {
				name = "email-" + name
			}

			if _, ok := names[name]; ok {
				return echo.NewHTTPError(http.StatusBadRequest,
					a.i18n.Ts("settings.duplicateMessengerName", "name", name))
			}

			names[name] = true
		}
		set.SMTP[i].Name = name

		// Assign a UUID. The frontend only sends a password when the user explicitly
		// changes the password. In other cases, the existing password in the DB
		// is copied while updating the settings and the UUID is used to match
		// the incoming array of SMTP blocks with the array in the DB.
		if s.UUID == "" {
			set.SMTP[i].UUID = uuid.Must(uuid.NewV4()).String()
		}

		// Ensure the HOST is trimmed of any whitespace.
		// This is a common mistake when copy-pasting SMTP settings.
		set.SMTP[i].Host = strings.TrimSpace(s.Host)

		// If there's no password coming in from the frontend, copy the existing
		// password by matching the UUID.
		if s.Password == "" {
			for _, c := range cur.SMTP {
				if s.UUID == c.UUID {
					set.SMTP[i].Password = c.Password
				}
			}
		}
	}
	if !has {
		return echo.NewHTTPError(http.StatusBadRequest, a.i18n.T("settings.errorNoSMTP"))
	}

	// Always remove the trailing slash from the app root URL.
	set.AppRootURL = strings.TrimRight(set.AppRootURL, "/")

	// Bounce boxes.
	for i, s := range set.BounceBoxes {
		// Assign a UUID. The frontend only sends a password when the user explicitly
		// changes the password. In other cases, the existing password in the DB
		// is copied while updating the settings and the UUID is used to match
		// the incoming array of blocks with the array in the DB.
		if s.UUID == "" {
			set.BounceBoxes[i].UUID = uuid.Must(uuid.NewV4()).String()
		}

		// Ensure the HOST is trimmed of any whitespace.
		// This is a common mistake when copy-pasting SMTP settings.
		set.BounceBoxes[i].Host = strings.TrimSpace(s.Host)

		if d, _ := time.ParseDuration(s.ScanInterval); d.Minutes() < 1 {
			return echo.NewHTTPError(http.StatusBadRequest, a.i18n.T("settings.bounces.invalidScanInterval"))
		}

		// If there's no password coming in from the frontend, copy the existing
		// password by matching the UUID.
		if s.Password == "" {
			for _, c := range cur.BounceBoxes {
				if s.UUID == c.UUID {
					set.BounceBoxes[i].Password = c.Password
				}
			}
		}
	}

	for i, m := range set.Messengers {
		// UUID to keep track of password changes similar to the SMTP logic above.
		if m.UUID == "" {
			set.Messengers[i].UUID = uuid.Must(uuid.NewV4()).String()
		}

		if m.Password == "" {
			for _, c := range cur.Messengers {
				if m.UUID == c.UUID {
					set.Messengers[i].Password = c.Password
				}
			}
		}

		name := reAlphaNum.ReplaceAllString(strings.ToLower(m.Name), "")
		if _, ok := names[name]; ok {
			return echo.NewHTTPError(http.StatusBadRequest,
				a.i18n.Ts("settings.duplicateMessengerName", "name", name))
		}
		if len(name) == 0 {
			return echo.NewHTTPError(http.StatusBadRequest, a.i18n.T("settings.invalidMessengerName"))
		}

		set.Messengers[i].Name = name
		names[name] = true
	}

	// S3 password?
	if set.UploadS3AwsSecretAccessKey == "" {
		set.UploadS3AwsSecretAccessKey = cur.UploadS3AwsSecretAccessKey
	}
	if set.SendgridKey == "" {
		set.SendgridKey = cur.SendgridKey
	}
	if set.BouncePostmark.Password == "" {
		set.BouncePostmark.Password = cur.BouncePostmark.Password
	}
	if set.BounceForwardEmail.Key == "" {
		set.BounceForwardEmail.Key = cur.BounceForwardEmail.Key
	}
	if set.SecurityCaptcha.HCaptcha.Secret == "" {
		set.SecurityCaptcha.HCaptcha.Secret = cur.SecurityCaptcha.HCaptcha.Secret
	}
	if set.OIDC.ClientSecret == "" {
		set.OIDC.ClientSecret = cur.OIDC.ClientSecret
	}

	// OIDC user auto-creation is enabled. Validate.
	if set.OIDC.AutoCreateUsers {
		if set.OIDC.DefaultUserRoleID.Int < auth.SuperAdminRoleID {
			return echo.NewHTTPError(http.StatusBadRequest,
				a.i18n.Ts("globals.messages.invalidFields", "name", a.i18n.T("settings.security.OIDCDefaultRole")))
		}
	}

	for n, v := range set.UploadExtensions {
		set.UploadExtensions[n] = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(v), "."))
	}

	// Domain blocklist / allowlist.
	doms := make([]string, 0, len(set.DomainBlocklist))
	for _, d := range set.DomainBlocklist {
		if d = strings.TrimSpace(strings.ToLower(d)); d != "" {
			doms = append(doms, d)
		}
	}
	set.DomainBlocklist = doms

	doms = make([]string, 0, len(set.DomainAllowlist))
	for _, d := range set.DomainAllowlist {
		if d = strings.TrimSpace(strings.ToLower(d)); d != "" {
			doms = append(doms, d)
		}
	}
	set.DomainAllowlist = doms

	// Validate and clean CORS domains.
	cors := make([]string, 0, len(set.SecurityCORSOrigins))
	for _, d := range set.SecurityCORSOrigins {
		if d = strings.TrimSpace(d); d != "" {
			if d == "*" {
				cors = append(cors, d)
				continue
			}

			// Parse and validate the URL.
			u, err := url.Parse(d)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return echo.NewHTTPError(http.StatusBadRequest,
					a.i18n.Ts("globals.messages.invalidData")+": invalid CORS domain: "+d)
			}
			// Save clean scheme + host
			cors = append(cors, u.Scheme+"://"+u.Host)
		}
	}
	set.SecurityCORSOrigins = cors

	// Validate slow query caching cron.
	if set.CacheSlowQueries {
		if _, err := cron.ParseStandard(set.CacheSlowQueriesInterval); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, a.i18n.Ts("globals.messages.invalidData")+": slow query cron: "+err.Error())
		}
	}

	// Update the settings in the DB.
	if err := a.core.UpdateSettings(set); err != nil {
		return err
	}

	// Re-initialize messengers with updated settings if no campaigns are running.
	// This allows SMTP settings to take effect immediately without requiring a full restart.
	if !a.manager.HasRunningCampaigns() {
		if err := a.reloadMessengers(); err != nil {
			a.log.Printf("error reloading messengers: %v", err)
			// Continue with restart if reload fails
		} else {
			// Successfully reloaded messengers, no need for full restart
			return c.JSON(http.StatusOK, okResp{true})
		}
	}

	return a.handleSettingsRestart(c)
}

// UpdateSettingsByKey updates a single setting key-value in the DB.
func (a *App) UpdateSettingsByKey(c echo.Context) error {
	key := c.Param("key")
	if key == "" {
		return echo.NewHTTPError(http.StatusBadRequest, a.i18n.T("globals.messages.invalidData"))
	}

	// Read the raw JSON body as the value.
	var b json.RawMessage
	if err := c.Bind(&b); err != nil {
		return err
	}

	// Update the value in the DB.
	if err := a.core.UpdateSettingsByKey(key, b); err != nil {
		return err
	}

	return a.handleSettingsRestart(c)
}

// handleSettingsRestart checks for running campaigns and either triggers an
// immediate app restart or marks the app as needing a restart.
func (a *App) handleSettingsRestart(c echo.Context) error {
	// If there are any active campaigns, don't do an auto reload and
	// warn the user on the frontend.
	if a.manager.HasRunningCampaigns() {
		a.Lock()
		a.needsRestart = true
		a.Unlock()

		return c.JSON(http.StatusOK, okResp{struct {
			NeedsRestart bool `json:"needs_restart"`
		}{true}})
	}

	// No running campaigns. Reload the app.
	go func() {
		<-time.After(time.Millisecond * 500)
		a.chReload <- syscall.SIGHUP
	}()

	return c.JSON(http.StatusOK, okResp{true})
}

// reloadMessengers re-initializes messengers from the updated settings.
// This allows SMTP settings to take effect immediately without requiring a full restart.
func (a *App) reloadMessengers() error {
	// Close old messengers.
	for _, m := range a.messengers {
		if err := m.Close(); err != nil {
			a.log.Printf("error closing messenger %s: %v", m.Name(), err)
		}
	}

	// Reload settings from DB to get updated SMTP config.
	settings, err := a.core.GetSettings()
	if err != nil {
		return fmt.Errorf("error loading settings: %v", err)
	}

	// Re-initialize SMTP messengers.
	var servers []email.Server
	var newMsgrs []manager.Messenger

	for _, s := range settings.SMTP {
		if !s.Enabled {
			continue
		}

		// Convert EmailHeaders from []map[string]string to map[string]string
		// before marshaling, as email.Server expects map[string]string.
		emailHeaders := make(map[string]string)
		if len(s.EmailHeaders) > 0 && len(s.EmailHeaders[0]) > 0 {
			emailHeaders = s.EmailHeaders[0]
		}

		// Create a temporary struct with the correct EmailHeaders type for marshaling.
		type smtpTemp struct {
			Name          string            `json:"name"`
			Host          string            `json:"host"`
			Port          int               `json:"port"`
			AuthProtocol  string            `json:"auth_protocol"`
			Username      string            `json:"username"`
			Password      string            `json:"password"`
			EmailHeaders  map[string]string `json:"email_headers"`
			MaxConns      int               `json:"max_conns"`
			MaxMsgRetries int               `json:"max_msg_retries"`
			IdleTimeout   string            `json:"idle_timeout"`
			WaitTimeout   string            `json:"wait_timeout"`
			TLSType       string            `json:"tls_type"`
			TLSSkipVerify bool              `json:"tls_skip_verify"`
			HelloHostname string            `json:"hello_hostname"`
		}

		temp := smtpTemp{
			Name:          s.Name,
			Host:          s.Host,
			Port:          s.Port,
			AuthProtocol:  s.AuthProtocol,
			Username:      s.Username,
			Password:      s.Password,
			EmailHeaders:  emailHeaders,
			MaxConns:      s.MaxConns,
			MaxMsgRetries: s.MaxMsgRetries,
			IdleTimeout:   s.IdleTimeout,
			WaitTimeout:   s.WaitTimeout,
			TLSType:       s.TLSType,
			TLSSkipVerify: s.TLSSkipVerify,
			HelloHostname: s.HelloHostname,
		}

		// Convert to JSON and then unmarshal into email.Server
		// This ensures proper handling of embedded smtppool.Opt fields.
		sJSON, err := json.Marshal(temp)
		if err != nil {
			return fmt.Errorf("error marshaling SMTP settings: %v", err)
		}

		// Use koanf to unmarshal, similar to initSMTPMessengers.
		koSrv := koanf.New(".")
		if err := koSrv.Load(rawbytes.Provider(sJSON), koanfjson.Parser()); err != nil {
			return fmt.Errorf("error loading SMTP config: %v", err)
		}

		var srv email.Server
		if err := koSrv.UnmarshalWithConf("", &srv, koanf.UnmarshalConf{Tag: "json"}); err != nil {
			return fmt.Errorf("error unmarshaling SMTP config: %v", err)
		}

		servers = append(servers, srv)
		a.log.Printf("re-initialized email (SMTP) messenger: %s@%s", s.Username, s.Host)

		// If the server has a name, initialize it as a standalone e-mail messenger.
		if srv.Name != "" {
			msgr, err := email.New(srv.Name, srv)
			if err != nil {
				return fmt.Errorf("error initializing e-mail messenger %s: %v", srv.Name, err)
			}
			newMsgrs = append(newMsgrs, msgr)
		}
	}

	// Initialize the 'email' messenger with all SMTP servers.
	if len(servers) > 0 {
		msgr, err := email.New(email.MessengerName, servers...)
		if err != nil {
			return fmt.Errorf("error initializing e-mail messenger: %v", err)
		}

		// If it's just one server, return only the default "email" messenger.
		if len(servers) == 1 {
			newMsgrs = []manager.Messenger{msgr}
		} else {
			// If there are multiple servers, prepend the group "email" to be the first one.
			newMsgrs = append([]manager.Messenger{msgr}, newMsgrs...)
		}
	}

	// Re-initialize postback messengers from global ko config.
	// Note: Postback messengers are loaded from config, not from DB settings,
	// so we use the global ko variable.
	postbackMsgrs := initPostbackMessengers(ko)
	newMsgrs = append(newMsgrs, postbackMsgrs...)

	// Clear old messengers from manager and add new ones.
	a.manager.ClearMessengers()
	for _, m := range newMsgrs {
		if err := a.manager.AddMessenger(m); err != nil {
			return fmt.Errorf("error adding messenger %s: %v", m.Name(), err)
		}
	}

	// Update app's messenger list.
	a.messengers = newMsgrs

	// Update emailMsgr if it exists.
	for _, m := range newMsgrs {
		if m.Name() == "email" {
			if em, ok := m.(*email.Emailer); ok {
				a.emailMsgr = em
			}
		}
	}

	return nil
}

// GetLogs returns the log entries stored in the log buffer.
func (a *App) GetLogs(c echo.Context) error {
	return c.JSON(http.StatusOK, okResp{a.bufLog.Lines()})
}

// TestSMTPSettings returns the log entries stored in the log buffer.
func (a *App) TestSMTPSettings(c echo.Context) error {
	// Copy the raw JSON post body.
	reqBody, err := io.ReadAll(c.Request().Body)
	if err != nil {
		a.log.Printf("error reading SMTP test: %v", err)
		return echo.NewHTTPError(http.StatusBadRequest, a.i18n.Ts("globals.messages.internalError"))
	}

	// Load the JSON into koanf to parse SMTP settings properly including timestrings.
	ko := koanf.New(".")
	if err := ko.Load(rawbytes.Provider(reqBody), koanfjson.Parser()); err != nil {
		a.log.Printf("error unmarshalling SMTP test request: %v", err)
		return echo.NewHTTPError(http.StatusBadRequest, a.i18n.Ts("globals.messages.internalError"))
	}

	req := email.Server{}
	if err := ko.UnmarshalWithConf("", &req, koanf.UnmarshalConf{Tag: "json"}); err != nil {
		a.log.Printf("error scanning SMTP test request: %v", err)
		return echo.NewHTTPError(http.StatusBadRequest, a.i18n.Ts("globals.messages.internalError"))
	}

	// UUID to fetch existing password if it's masked.
	uuid := ko.String("uuid")
	if uuid != "" && isMasked(req.Password) {
		cur, err := a.core.GetSettings()
		if err != nil {
			return err
		}

		for _, s := range cur.SMTP {
			if s.UUID == uuid {
				req.Password = s.Password
				break
			}
		}
	}

	if strings.HasSuffix(strings.ToLower(req.Host), "gmail.com") {
		req.Password = strings.ReplaceAll(req.Password, " ", "")
	}

	to := ko.String("email")
	if to == "" {
		return echo.NewHTTPError(http.StatusBadRequest, a.i18n.Ts("globals.messages.missingFields", "name", "email"))
	}

	// Initialize a new SMTP pool.
	req.MaxConns = 1
	req.IdleTimeout = time.Second * 2
	req.PoolWaitTimeout = time.Second * 2
	msgr, err := email.New("", req)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest,
			a.i18n.Ts("globals.messages.errorCreating", "name", "SMTP", "error", err.Error()))
	}

	// Render the test email template body.
	var b bytes.Buffer
	if err := notifs.Tpls.ExecuteTemplate(&b, "smtp-test", nil); err != nil {
		a.log.Printf("error compiling notification template '%s': %v", "smtp-test", err)
		return err
	}

	m := models.Message{}
	m.From = a.cfg.FromEmail
	m.To = []string{to}
	m.Subject = a.i18n.T("settings.smtp.testConnection")
	m.Body = b.Bytes()

	a.log.Printf("sending test email from %s to %s via %s:%d", m.From, to, req.Host, req.Port)

	if err := msgr.Push(m); err != nil {
		a.log.Printf("error sending test email: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}

	a.log.Printf("test email sent successfully to %s", to)
	return c.JSON(http.StatusOK, okResp{a.bufLog.Lines()})
}

func (a *App) GetAboutInfo(c echo.Context) error {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	out := a.about
	out.System.AllocMB = mem.Alloc / 1024 / 1024
	out.System.OSMB = mem.Sys / 1024 / 1024

	return c.JSON(http.StatusOK, out)
}

func isMasked(pwd string) bool {
	return strings.Contains(pwd, pwdMask)
}
