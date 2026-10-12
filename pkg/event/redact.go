package event

import (
	"errors"
	"net/url"
	"regexp"
)

// RedactURL keeps a URL's scheme and host. Webhook URLs carry their
// credential in the path (Slack, Feishu) or the query (DingTalk's
// access_token, Telegram relays), so nothing past the host is safe to log
// or store.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "<url>"
	}
	return u.Scheme + "://" + u.Host
}

var urlInText = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s"'<>]+`)

// RedactText runs RedactURL over every URL in s, for error messages that
// quote one.
func RedactText(s string) string {
	return urlInText.ReplaceAllStringFunc(s, RedactURL)
}

// redactRequestError cuts the URL that net/http quotes in every transport
// error ("Post \"https://…?access_token=…\": dial tcp …") down to its host,
// in place, so the error keeps its type for errors.As and stays clean in
// logs, in a channel's last error and wherever it is shown.
func redactRequestError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		ue.URL = RedactURL(ue.URL)
	}
	return err
}

// RedactRequestError is redactRequestError for other packages that post to
// a URL holding a credential.
func RedactRequestError(err error) error { return redactRequestError(err) }
