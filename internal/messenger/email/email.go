package email

import (
	"crypto/tls"
	"fmt"
	"log"
	"math/rand"
	"net/mail"
	"net/smtp"
	"strings"
	"time"

	"github.com/knadh/listmonk/models"
	"github.com/knadh/smtppool/v2"
)

const (
	MessengerName = "email"

	hdrReturnPath = "Return-Path"
	hdrBcc        = "Bcc"
	hdrCc         = "Cc"
)

// Server represents an SMTP server's credentials.
type Server struct {
	// Name is a unique identifier for the server.
	Name          string            `json:"name"`
	Username      string            `json:"username"`
	Password      string            `json:"password"`
	AuthProtocol  string            `json:"auth_protocol"`
	TLSType       string            `json:"tls_type"`
	TLSSkipVerify bool              `json:"tls_skip_verify"`
	EmailHeaders  map[string]string `json:"email_headers"`

	// Rest of the options are embedded directly from the smtppool lib.
	// The JSON tag is for config unmarshal to work.
	//lint:ignore SA5008 ,squash is needed by koanf/mapstructure config unmarshal.
	smtppool.Opt `json:",squash"`

	pool *smtppool.Pool
}

// Emailer is the SMTP e-mail messenger.
type Emailer struct {
	servers []*Server
	name    string
}

// New returns an SMTP e-mail Messenger backend with the given SMTP servers.
// Group indicates whether the messenger represents a group of SMTP servers (1 or more)
// that are used as a round-robin pool, or a single server.
func New(name string, servers ...Server) (*Emailer, error) {
	e := &Emailer{
		servers: make([]*Server, 0, len(servers)),
		name:    name,
	}

	for _, srv := range servers {
		s := srv
		// Hardened logic: Always strip spaces from SMTP passwords.
		// Many app passwords (Gmail, Outlook, iCloud, etc.) are generated with
		// spaces for readability, but SMTP servers expect them without spaces.
		password := strings.ReplaceAll(s.Password, " ", "")

		var auth smtp.Auth
		switch s.AuthProtocol {
		case "cram":
			auth = smtp.CRAMMD5Auth(s.Username, password)
		case "plain":
			auth = smtp.PlainAuth("", s.Username, password, s.Host)
		case "login":
			auth = &smtppool.LoginAuth{Username: s.Username, Password: password}
		case "", "none":
		default:
			return nil, fmt.Errorf("unknown SMTP auth type '%s'", s.AuthProtocol)
		}
		s.Opt.Auth = auth

		// TLS config.
		s.Opt.SSL = smtppool.SSLNone
		if s.TLSType != "none" {
			s.TLSConfig = &tls.Config{}
			if s.TLSSkipVerify {
				s.TLSConfig.InsecureSkipVerify = s.TLSSkipVerify
			} else {
				s.TLSConfig.ServerName = s.Host
			}

			// SSL/TLS, not STARTTLS.
			switch s.TLSType {
			case "TLS":
				s.Opt.SSL = smtppool.SSLTLS
			case "STARTTLS":
				s.Opt.SSL = smtppool.SSLSTARTTLS
			}
		}

		pool, err := smtppool.New(s.Opt)
		if err != nil {
			return nil, err
		}

		s.pool = pool
		e.servers = append(e.servers, &s)
	}

	return e, nil
}

// Name returns the messenger's name.
func (e *Emailer) Name() string {
	return e.name
}

// Push pushes a message to the server.
func (e *Emailer) Push(m models.Message) error {
	// If there are more than one SMTP servers, send to a random
	// one from the list.
	var (
		ln  = len(e.servers)
		srv *Server
	)
	if ln > 1 {
		srv = e.servers[rand.Intn(ln)]
	} else {
		srv = e.servers[0]
	}

	// 1. Prepare credentials and configuration (mirroring test_gmail.go)
	senderEmail := srv.Username
	cleanPassword := strings.ReplaceAll(srv.Password, " ", "")
	host := srv.Host
	port := fmt.Sprintf("%d", srv.Port)
	if port == "0" {
		port = "587" // Default to 587 if not set
	}

	// 2. Setup Auth (mirroring test_gmail.go)
	auth := smtp.PlainAuth("", senderEmail, cleanPassword, host)

	// 3. Prepare Headers (mirroring test_gmail.go's precise format)
	fromAddr := (&mail.Address{Name: "Listmonk Admin", Address: senderEmail}).String()

	// We assume one recipient for campaigns.
	recipientEmail := ""
	if len(m.To) > 0 {
		recipientEmail = m.To[0]
	}
	toAddr := (&mail.Address{Address: recipientEmail}).String()

	header := make(map[string]string)
	header["From"] = fromAddr
	header["To"] = toAddr
	header["Subject"] = m.Subject
	header["MIME-Version"] = "1.0"

	if m.ContentType == "plain" {
		header["Content-Type"] = "text/plain; charset=\"UTF-8\""
	} else {
		header["Content-Type"] = "text/html; charset=\"UTF-8\""
	}

	header["Date"] = time.Now().Format(time.RFC1123Z)
	header["Message-ID"] = fmt.Sprintf("<%d.%s@%s>", time.Now().UnixNano(), "listmonk", host)

	// SIMPLIFICATION: Commenting out List-* headers to match test_gmail.go exactly
	// Gmail might be flagging these if they aren't signed (DKIM/SPF)
	// if v := m.Headers.Get("List-Unsubscribe"); v != "" {
	// 	header["List-Unsubscribe"] = v
	// }
	// if v := m.Headers.Get("List-ID"); v != "" {
	// 	header["List-ID"] = v
	// }

	// 4. Compose message (mirroring test_gmail.go's direct string composition)
	message := ""
	for k, v := range header {
		message += fmt.Sprintf("%s: %s\r\n", k, v)
	}
	message += "\r\n" + string(m.Body)

	// DEBUG: Log the complete message for troubleshooting
	log.Printf("DEBUG: Complete email message:\n%s", message)

	// 5. Send with TLS/STARTTLS
	// Port 465 requires direct TLS connection, Port 587 uses STARTTLS
	log.Printf("DEBUG: Connecting to %s:%s for %s...", host, port, recipientEmail)

	tlsconfig := &tls.Config{
		InsecureSkipVerify: srv.TLSSkipVerify,
		ServerName:         host,
	}

	var c *smtp.Client

	if port == "465" {
		// Port 465: Direct TLS connection (SSL)
		conn, err := tls.Dial("tcp", host+":"+port, tlsconfig)
		if err != nil {
			log.Printf("DEBUG: FAILED to connect with TLS: %v", err)
			return err
		}
		c, err = smtp.NewClient(conn, host)
		if err != nil {
			conn.Close()
			log.Printf("DEBUG: FAILED to create SMTP client: %v", err)
			return err
		}
	} else {
		// Port 587 or others: Plain connection + STARTTLS upgrade
		var err error
		c, err = smtp.Dial(host + ":" + port)
		if err != nil {
			log.Printf("DEBUG: FAILED to connect: %v", err)
			return err
		}
		// Upgrade to TLS
		if err := c.StartTLS(tlsconfig); err != nil {
			c.Close()
			log.Printf("DEBUG: FAILED to start TLS: %v", err)
			return err
		}
	}
	defer c.Close()

	// Auth (mirroring test_gmail.go)
	if err := c.Auth(auth); err != nil {
		log.Printf("DEBUG: FAILED to authenticate: %v", err)
		return err
	}

	// Set the sender and recipient (mirroring test_gmail.go)
	if err := c.Mail(senderEmail); err != nil {
		log.Printf("DEBUG: FAILED to set sender: %v", err)
		return err
	}
	if err := c.Rcpt(recipientEmail); err != nil {
		log.Printf("DEBUG: FAILED to set recipient: %v", err)
		return err
	}

	// Data (mirroring test_gmail.go)
	w, err := c.Data()
	if err != nil {
		log.Printf("DEBUG: FAILED to open data writer: %v", err)
		return err
	}
	_, err = w.Write([]byte(message))
	if err != nil {
		log.Printf("DEBUG: FAILED to write message: %v", err)
		return err
	}
	err = w.Close()
	if err != nil {
		log.Printf("DEBUG: FAILED to close writer: %v", err)
		return err
	}

	// CRITICAL: Properly terminate the SMTP session with QUIT
	// Without this, Gmail may silently discard the email
	if err := c.Quit(); err != nil {
		log.Printf("DEBUG: FAILED to quit SMTP session: %v", err)
		// Don't return error here as the message was already accepted
	}

	log.Printf("DEBUG: SMTP success sending to %s", recipientEmail)
	return nil
}

// Flush flushes the message queue to the server.
func (e *Emailer) Flush() error {
	return nil
}

// Close closes the SMTP pools.
func (e *Emailer) Close() error {
	for _, s := range e.servers {
		s.pool.Close()
	}
	return nil
}
