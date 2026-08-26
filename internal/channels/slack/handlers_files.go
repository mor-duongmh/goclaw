package slack

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
)

// --- Message debounce/batching ---

type debounceEntry struct {
	timer      *time.Timer
	messages   []string
	mu         sync.Mutex
	senderID   string
	channelID  string
	media      []string
	metadata   map[string]string
	peerKind   string
	authorized bool
}

// debounceMessage batches rapid messages. Returns true if message was debounced.
func (c *Channel) debounceMessage(localKey, senderID, channelID, content string, media []string, metadata map[string]string, peerKind string, authorized bool) bool {
	c.debounceMu.Lock()
	entry, loaded := c.debounceTimers[localKey]
	if !loaded {
		entry = &debounceEntry{
			senderID:   senderID,
			channelID:  channelID,
			media:      media,
			metadata:   metadata,
			peerKind:   peerKind,
			authorized: authorized,
		}
		c.debounceTimers[localKey] = entry
	}
	c.debounceMu.Unlock()

	entry.mu.Lock()
	defer entry.mu.Unlock()

	entry.messages = append(entry.messages, content)
	if loaded {
		// Only append media for subsequent messages; first message's media is set in constructor.
		entry.media = append(entry.media, media...)
	}
	entry.metadata = metadata // use latest message's metadata

	if !loaded {
		entry.timer = time.AfterFunc(c.debounceDelay, func() {
			c.flushDebounce(localKey)
		})
		return true
	}

	if entry.timer != nil {
		entry.timer.Reset(c.debounceDelay)
	}
	return true
}

func (c *Channel) flushDebounce(localKey string) {
	c.debounceMu.Lock()
	entry, ok := c.debounceTimers[localKey]
	if ok {
		delete(c.debounceTimers, localKey)
	}
	c.debounceMu.Unlock()

	if !ok {
		return
	}

	entry.mu.Lock()
	combined := strings.Join(entry.messages, "\n")
	entry.mu.Unlock()

	c.publishMessage(entry.senderID, entry.channelID, combined, entry.media, entry.metadata, entry.peerKind, entry.authorized)

	if entry.peerKind == "group" {
		c.GroupHistory().Clear(localKey)
	}
}

// --- File download (SSRF-protected) ---

var slackDownloadAllowlist = []string{
	".slack.com",
	".slack-edge.com",
	".slack-files.com",
}

func isAllowedDownloadHost(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, suffix := range slackDownloadAllowlist {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

func (c *Channel) downloadFile(name, urlPrivate, urlPrivateDownload string, maxBytes int64) (string, error) {
	downloadURL := urlPrivateDownload
	if downloadURL == "" {
		downloadURL = urlPrivate
	}
	if downloadURL == "" {
		return "", fmt.Errorf("no download URL for file %s", name)
	}

	if !isAllowedDownloadHost(downloadURL) {
		return "", fmt.Errorf("security: download URL hostname not in Slack allowlist: %s", downloadURL)
	}

	ext := filepath.Ext(name)
	if ext == "" {
		ext = ".dat"
	}
	tmpFile, err := os.CreateTemp("", "slack-file-*"+ext)
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	defer tmpFile.Close()

	client := &http.Client{
		Timeout: 60 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			req.Header.Del("Authorization") // strip auth on redirect (CDN presigned URL)
			if req.URL.Scheme != "https" {
				return fmt.Errorf("security: redirect to non-HTTPS URL blocked: %s", req.URL)
			}
			// Only allow redirects to known Slack CDN domains to prevent SSRF.
			host := req.URL.Hostname()
			if !isAllowedSlackHost(host) {
				return fmt.Errorf("security: redirect to untrusted host blocked: %s", host)
			}
			if len(via) >= 3 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}

	req, err := http.NewRequest("GET", downloadURL, nil)
	if err != nil {
		os.Remove(tmpFile.Name())
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.config.BotToken)

	resp, err := client.Do(req)
	if err != nil {
		os.Remove(tmpFile.Name())
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		os.Remove(tmpFile.Name())
		return "", fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	if _, err := io.Copy(tmpFile, io.LimitReader(resp.Body, maxBytes)); err != nil {
		os.Remove(tmpFile.Name())
		return "", err
	}

	return tmpFile.Name(), nil
}

// allowedSlackHosts contains trusted Slack CDN domains for redirect validation.
var allowedSlackHosts = []string{
	".slack-edge.com",
	".slack.com",
	"files.slack.com",
}

// isAllowedSlackHost checks if a hostname belongs to a known Slack CDN domain.
func isAllowedSlackHost(host string) bool {
	for _, suffix := range allowedSlackHosts {
		if host == strings.TrimPrefix(suffix, ".") || strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// --- File upload (v2 3-step API) ---

func (c *Channel) uploadFile(ctx context.Context, channelID, threadTS string, media bus.MediaAttachment) error {
	params, err := uploadParams(channelID, threadTS, media)
	if err != nil {
		return err
	}

	// No deadline of its own: every attachment of a reply shares the one budget
	// Send opened, so N attachments cannot cost N upload budgets. Uploads stream
	// their body, so slack-go will not retry them either.
	if _, err := c.api.UploadFileContext(ctx, params); err != nil {
		return fmt.Errorf("upload file: %w", err)
	}

	return nil
}

// uploadParams describes one attachment to slack-go, streaming the body from
// disk instead of reading the whole file into memory first.
//
// That is not just about the allocation. slack-go's multipart writer runs in a
// goroutine that reports on an UNBUFFERED channel which the caller stops reading
// as soon as the request itself fails (misc.go postWithMultipartResponse), so an
// upload cut short by a deadline or a cancel leaks that goroutine forever,
// holding whatever reader it was handed. Handing it a path makes the library
// open and close its own *os.File, so the leaked goroutine retains a closed file
// handle rather than every byte of the attachment.
func uploadParams(channelID, threadTS string, media bus.MediaAttachment) (slackapi.UploadFileParameters, error) {
	info, err := os.Stat(media.URL)
	if err != nil {
		return slackapi.UploadFileParameters{}, fmt.Errorf("stat file %s: %w", media.URL, err)
	}
	fileName := filepath.Base(media.URL)
	return slackapi.UploadFileParameters{
		File:            media.URL,
		Filename:        fileName,
		FileSize:        int(info.Size()),
		Title:           fileName,
		InitialComment:  media.Caption,
		Channel:         channelID,
		ThreadTimestamp: threadTS,
	}, nil
}
