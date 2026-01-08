package mailbox

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset"
	"github.com/gofrs/uuid/v5"
	"github.com/knadh/go-pop3"
	"github.com/knadh/listmonk/models"
)

// Helper functions for min/max
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// isValidUUID validates if a string is a valid UUID format
func isValidUUID(s string) bool {
	if s == "" {
		return false
	}
	_, err := uuid.FromString(s)
	return err == nil
}

// POP represents a POP mailbox.
type POP struct {
	opt    Opt
	client *pop3.Client
}

type bounceHeaders struct {
	Header string
	Regexp *regexp.Regexp
}

type bounceMeta struct {
	From           string   `json:"from"`
	Subject        string   `json:"subject"`
	MessageID      string   `json:"message_id"`
	DeliveredTo    string   `json:"delivered_to"`
	Received       []string `json:"received"`
	ClassifyReason string   `json:"classify_reason"`
}

var (
	// List of header to look for in the e-mail body, regexp to fall back to if the header is empty.
	headerLookups = []bounceHeaders{
		// Enhanced regex patterns to find Campaign UUID in various formats within bounce emails
		{models.EmailHeaderCampaignUUID, regexp.MustCompile(`(?i)(?:^|\s|>|"|'|` + models.EmailHeaderCampaignUUID + `[:\s]+)([a-z0-9\-]{36})(?:<|"|'|\s|$|,|;|$)`)},
		{models.EmailHeaderSubscriberUUID, regexp.MustCompile(`(?i)(?:^|\s|>|"|'|` + models.EmailHeaderSubscriberUUID + `[:\s]+)([a-z0-9\-]{36})(?:<|"|'|\s|$|,|;|$)`)},
		{models.EmailHeaderDate, regexp.MustCompile(`(?m)(?:^` + models.EmailHeaderDate + `:\s+?)([\w,\,\ ,:,+,-]*(?:\(?:\w*\))?)`)},
		{models.EmailHeaderFrom, regexp.MustCompile(`(?m)(?:^` + models.EmailHeaderFrom + `:\s+?)(.*)`)},
		{models.EmailHeaderSubject, regexp.MustCompile(`(?m)(?:^` + models.EmailHeaderSubject + `:\s+?)(.*)`)},
		{models.EmailHeaderMessageId, regexp.MustCompile(`(?m)(?:^` + models.EmailHeaderMessageId + `:\s+?)(.*)`)},
		{models.EmailHeaderDeliveredTo, regexp.MustCompile(`(?m)(?:^` + models.EmailHeaderDeliveredTo + `:\s+?)(.*)`)},
	}

	reHdrReceived = regexp.MustCompile(`(?m)(?:^` + models.EmailHeaderReceived + `:\s+?)(.*)`)

	// SMTP status code (5.x.x or 4.x.x) to classify hard/soft bounces.
	reSMTPStatus = regexp.MustCompile(`(?m)(?i)^(?:Status:\s*)?(?:\d{3}\s+)?([45]\.\d+\.\d+)`)

	// List of (conventional) strings to guess hard bounces.
	reHardBounce = regexp.MustCompile(`(?i)(NXDOMAIN|user unknown|address not found|mailbox not found|address.*reject|does not exist|` +
		`invalid recipient|no such user|recipient.*invalid|undeliverable|permanent.*failure|permanent.*error|` +
		`bad.*address|unknown.*user|account.*disabled|address.*disabled)`)
)

// NewPOP returns a new instance of the POP mailbox client.
func NewPOP(opt Opt) *POP {
	return &POP{
		opt: opt,
		client: pop3.New(pop3.Opt{
			Host:          opt.Host,
			Port:          opt.Port,
			TLSEnabled:    opt.TLSEnabled,
			TLSSkipVerify: opt.TLSSkipVerify,
		}),
	}
}

// classifyBounce analyzes the bounce message content and determines if it's a hard or soft bounce.
// It checks SMTP status codes, diagnostic headers, and bounce keywords (using string heuristics).
// soft is the default preference.
// Returns the bounce type and a classification reason containing context about what matched.
func classifyBounce(b []byte) (string, string) {
	if matches := reSMTPStatus.FindAllSubmatch(b, -1); matches != nil {
		for _, m := range matches {
			if len(m) >= 2 && len(m[0]) > 1 {
				// Full status code (e.g., "5.1.1").
				status := m[1]

				// 5.x.x is hard bounce.
				if status[0] == '5' {
					return models.BounceTypeHard, fmt.Sprintf("smtp_status=%s", status)
				}

				// 4.x.x  is soft bounce.
				if status[0] == '4' {
					return models.BounceTypeSoft, fmt.Sprintf("smtp_status=%s", status)
				}
			}
		}
	}

	// Check for explicit hard bounce keywords.
	if match := reHardBounce.FindSubmatch(b); match != nil {
		return models.BounceTypeHard, fmt.Sprintf("body_match=%s", match[1])
	}

	return models.BounceTypeSoft, "default"
}

// Scan scans the mailbox and pushes the downloaded messages into the given channel.
// The messages that are downloaded are deleted from the server. If limit > 0,
// all messages on the server are downloaded and deleted.
func (p *POP) Scan(limit int, ch chan models.Bounce) error {
	c, err := p.client.NewConn()
	if err != nil {
		return err
	}
	defer c.Quit()

	// Authenticate.
	if p.opt.AuthProtocol != "none" {
		if err := c.Auth(p.opt.Username, p.opt.Password); err != nil {
			return err
		}
	}

	// Get the total number of messages on the server.
	count, _, err := c.Stat()
	if err != nil {
		return err
	}

	// No messages.
	if count == 0 {
		return nil
	}

	if limit > 0 && count > limit {
		count = limit
	}

	// Download messages.
	for id := 1; id <= count; id++ {
		// Retrieve the raw bytes of the message.
		b, err := c.RetrRaw(id)
		if err != nil {
			return err
		}

		// Parse the message.
		m, err := message.Read(b)
		if err != nil {
			return err
		}

		h := m

		// If this is a multipart message, find the last part.
		if mr := m.MultipartReader(); mr != nil {
			for {
				part, err := mr.NextPart()
				if err == io.EOF {
					break
				} else if err != nil {
					return err
				}
				h = part
			}
		}

		// Reset the "unread portion" pointer of the message buffer.
		// If you don't do this, you can't read the entire body because the pointer will not point to the beginning.
		b, _ = c.RetrRaw(id)

		// Lookup headers in the e-mail. If a header isn't found, fall back to regexp lookups.
		hdr := make(map[string]string, 7)
		bodyBytes := b.Bytes()
		bodyStr := string(bodyBytes)

		for _, l := range headerLookups {
			v := h.Header.Get(l.Header)

			// Not in the header. Try regexp in the entire email body.
			if v == "" {
				matches := l.Regexp.FindAllSubmatch(bodyBytes, -1)
				if len(matches) > 0 {
					// Take the first match (most likely to be the original email's header)
					v = string(matches[0][1])
				}

				// For Campaign UUID, try enhanced search if still not found
				if l.Header == models.EmailHeaderCampaignUUID && v == "" {
					// Try case-insensitive search for UUID near campaign-related keywords
					bodyLower := strings.ToLower(bodyStr)
					uuidPattern := regexp.MustCompile(`([a-z0-9]{8}-[a-z0-9]{4}-[a-z0-9]{4}-[a-z0-9]{4}-[a-z0-9]{12})`)
					campaignKeywords := []string{"campaign", "x-listmonk-campaign", "listmonk"}
					for _, keyword := range campaignKeywords {
						keywordIdx := strings.Index(bodyLower, keyword)
						if keywordIdx >= 0 {
							// Find UUID near the keyword (within 200 chars)
							startIdx := max(0, keywordIdx-100)
							endIdx := min(len(bodyStr), keywordIdx+200)
							searchArea := bodyStr[startIdx:endIdx]
							uuidMatches := uuidPattern.FindAllString(strings.ToLower(searchArea), -1)
							if len(uuidMatches) > 0 {
								// Convert back to original case from original body
								uuidLower := uuidMatches[0]
								// Find the UUID in original case
								uuidIdx := strings.Index(strings.ToLower(bodyStr), uuidLower)
								if uuidIdx >= 0 {
									v = bodyStr[uuidIdx : uuidIdx+36]
									break
								}
							}
						}
					}
				}
			}

			// Validate UUID format for Campaign and Subscriber UUIDs
			trimmed := strings.TrimSpace(v)
			if l.Header == models.EmailHeaderCampaignUUID || l.Header == models.EmailHeaderSubscriberUUID {
				if !isValidUUID(trimmed) {
					// Invalid UUID format, set to empty string so fallback mechanism can work
					trimmed = ""
				}
			}

			hdr[l.Header] = trimmed
		}

		// Received is a []string header.
		msgReceived := h.Header.Map()[models.EmailHeaderReceived]
		if len(msgReceived) == 0 {
			if u := reHdrReceived.FindAllSubmatch(b.Bytes(), -1); u != nil {
				for i := 0; i < len(u); i++ {
					msgReceived = append(msgReceived, string(u[i][1]))
				}
			}
		}

		date, _ := time.Parse("Mon, 02 Jan 2006 15:04:05 -0700", hdr[models.EmailHeaderDate])
		if date.IsZero() {
			date = time.Now()
		}

		// Classify the bounce type based on message content.
		bounceType, bounceReason := classifyBounce(b.Bytes())

		// Additional bounce e-mail metadata.
		fmt.Println(bounceReason)
		meta, _ := json.Marshal(bounceMeta{
			From:           hdr[models.EmailHeaderFrom],
			Subject:        hdr[models.EmailHeaderSubject],
			MessageID:      hdr[models.EmailHeaderMessageId],
			DeliveredTo:    hdr[models.EmailHeaderDeliveredTo],
			Received:       msgReceived,
			ClassifyReason: bounceReason,
		})

		// Extract email address from bounce message
		// Priority: Final-Recipient > Original-Recipient > Delivered-To > body patterns
		email := ""

		// Try Final-Recipient header (most reliable for bounce emails)
		if finalRecipient := h.Header.Get("Final-Recipient"); finalRecipient != "" {
			emailMatch := regexp.MustCompile(`(?i)rfc822;\s*([a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,})`).FindStringSubmatch(finalRecipient)
			if len(emailMatch) > 1 {
				email = strings.ToLower(strings.TrimSpace(emailMatch[1]))
			} else {
				// Try simple email pattern
				emailMatch = regexp.MustCompile(`([a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,})`).FindStringSubmatch(finalRecipient)
				if len(emailMatch) > 1 {
					email = strings.ToLower(strings.TrimSpace(emailMatch[1]))
				}
			}
		}

		// Try Original-Recipient header
		if email == "" {
			if origRecipient := h.Header.Get("Original-Recipient"); origRecipient != "" {
				emailMatch := regexp.MustCompile(`(?i)rfc822;\s*([a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,})`).FindStringSubmatch(origRecipient)
				if len(emailMatch) > 1 {
					email = strings.ToLower(strings.TrimSpace(emailMatch[1]))
				} else {
					emailMatch = regexp.MustCompile(`([a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,})`).FindStringSubmatch(origRecipient)
					if len(emailMatch) > 1 {
						email = strings.ToLower(strings.TrimSpace(emailMatch[1]))
					}
				}
			}
		}

		// Try Delivered-To header (but skip if it's the bounce mailbox)
		if email == "" {
			deliveredTo := strings.ToLower(strings.TrimSpace(hdr[models.EmailHeaderDeliveredTo]))
			bounceMailbox := strings.ToLower(p.opt.Username)
			if deliveredTo != "" && deliveredTo != bounceMailbox {
				email = deliveredTo
			}
		}

		// Try to find recipient email in bounce message body (look for common bounce patterns)
		bounceMailbox := strings.ToLower(p.opt.Username)
		if email == "" {
			// Pattern 1: Final-Recipient in body
			emailPattern := regexp.MustCompile(`(?i)final[- ]?recipient[:\s]+(?:rfc822;)?\s*([a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,})`)
			emailMatches := emailPattern.FindAllStringSubmatch(bodyStr, -1)
			if len(emailMatches) > 0 {
				candidate := strings.ToLower(strings.TrimSpace(emailMatches[0][1]))
				if candidate != bounceMailbox {
					email = candidate
				}
			}
		}

		if email == "" {
			// Pattern 2: Original-Recipient in body
			emailPattern := regexp.MustCompile(`(?i)original[- ]?recipient[:\s]+(?:rfc822;)?\s*([a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,})`)
			emailMatches := emailPattern.FindAllStringSubmatch(bodyStr, -1)
			if len(emailMatches) > 0 {
				candidate := strings.ToLower(strings.TrimSpace(emailMatches[0][1]))
				if candidate != bounceMailbox {
					email = candidate
				}
			}
		}

		if email == "" {
			// Pattern 3: Generic recipient patterns (but exclude bounce mailbox)
			emailPattern := regexp.MustCompile(`(?i)(?:to|recipient|undelivered[^:]*to)[:\s]+([a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,})`)
			emailMatches := emailPattern.FindAllStringSubmatch(bodyStr, -1)
			for _, match := range emailMatches {
				candidate := strings.ToLower(strings.TrimSpace(match[1]))
				if candidate != bounceMailbox {
					email = candidate
					break
				}
			}
		}

		// Log extracted values for debugging
		fmt.Printf("Bounce detected - CampaignUUID: %s, SubscriberUUID: %s, Email: %s, Type: %s\n",
			hdr[models.EmailHeaderCampaignUUID], hdr[models.EmailHeaderSubscriberUUID],
			email, bounceType)

		select {
		case ch <- models.Bounce{
			Type:           bounceType,
			Email:          email,
			CampaignUUID:   hdr[models.EmailHeaderCampaignUUID],
			SubscriberUUID: hdr[models.EmailHeaderSubscriberUUID],
			Source:         p.opt.Host,
			CreatedAt:      date,
			Meta:           meta,
		}:
		default:
		}
	}

	// Delete the downloaded messages.
	for id := 1; id <= count; id++ {
		if err := c.Dele(id); err != nil {
			return err
		}
	}

	return nil
}
