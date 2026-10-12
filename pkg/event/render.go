package event

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Chat services do not accept an arbitrary JSON document. Each defines its own
// envelope and rejects anything else — Slack answers 400 "invalid_payload",
// Feishu answers 200 with a non-zero code in the body — so posting the raw
// Event to a bot URL delivers nothing at all, and does it quietly: the receiver
// answered, so from the sender's side it can look like it worked.
//
// That is why a receiver has a Kind. The event is the same; only the envelope
// around it differs.

// Kind is the message format a receiver expects.
type Kind string

const (
	// KindGeneric posts the Event JSON unchanged. This is the format for a
	// receiver you wrote yourself, and the only one that carries every field.
	KindGeneric Kind = "generic"
	// KindFeishu is a Feishu/Lark custom bot ("自定义机器人") webhook.
	KindFeishu Kind = "feishu"
	// KindSlack is a Slack incoming webhook.
	KindSlack Kind = "slack"
	// KindWeCom is a WeCom (企业微信) group bot webhook.
	KindWeCom Kind = "wecom"
	// KindDingTalk is a DingTalk (钉钉) custom robot webhook.
	KindDingTalk Kind = "dingtalk"
)

// ParseKind maps a configured string to a Kind.
//
// Unlike ParseSeverity, an unrecognised value is an ERROR rather than a
// default. A severity typo makes a receiver too chatty, which is visible; a
// kind typo would make every message silently unacceptable to the far end,
// which is not.
func ParseKind(s string) (Kind, error) {
	switch k := Kind(strings.ToLower(strings.TrimSpace(s))); k {
	case "":
		zap.L().Debug("ParseKind: empty kind, using generic")
		return KindGeneric, nil
	case KindGeneric, KindFeishu, KindSlack, KindWeCom, KindDingTalk:
		zap.L().Debug("ParseKind: kind recognised", zap.String("kind", string(k)))
		return k, nil
	default:
		zap.L().Debug("ParseKind: unknown kind rejected", zap.String("kind", s))
		return "", fmt.Errorf("event: unknown receiver kind %q (want generic, feishu, slack, wecom or dingtalk)", s)
	}
}

// Kinds lists every supported kind, for help text and UI menus.
func Kinds() []Kind {
	return []Kind{KindGeneric, KindFeishu, KindSlack, KindWeCom, KindDingTalk}
}

// NeedsSecret reports whether a kind can use a signing secret.
func (k Kind) NeedsSecret() bool { return k == KindDingTalk }

// wellKnownHosts maps a bot endpoint's host to the kind that endpoint requires.
//
// Suffix-matched, because each of these services publishes regional or
// tenant-specific hostnames under the same domain (larksuite.com is Feishu
// outside mainland China; DingTalk has oapi and api hosts).
var wellKnownHosts = map[string]Kind{
	"feishu.cn":       KindFeishu,
	"larksuite.com":   KindFeishu,
	"slack.com":       KindSlack,
	"weixin.qq.com":   KindWeCom,
	"dingtalk.com":    KindDingTalk,
	"aliyuncs.com":    KindDingTalk,
	"work.weixin.com": KindWeCom,
}

// KindForURL reports the kind a URL's host demands, when that is knowable.
//
// This exists because the most damaging mistake in this whole feature is
// silent: paste a Feishu bot URL, leave the kind on "generic", and the raw
// event JSON is posted, refused, and answered with HTTP 200. Nothing in the
// delivery path can tell that apart from success — a generic receiver's body is
// whatever the operator built, so it is not ours to interpret. The host,
// however, is unambiguous, and it is known before a single message is sent.
func KindForURL(rawURL string) (Kind, bool) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		zap.L().Debug("KindForURL: URL has no parsable host", zap.Bool("parse_failed", err != nil))
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	for suffix, kind := range wellKnownHosts {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			zap.L().Debug("KindForURL: host is a known chat service",
				zap.String("host", host), zap.String("suffix", suffix), zap.String("kind", string(kind)))
			return kind, true
		}
	}
	zap.L().Debug("KindForURL: host not a known chat service", zap.String("host", host))
	return "", false
}

// Render builds the request body for one event in this kind's format.
func (k Kind) Render(e Event) ([]byte, error) {
	zap.L().Debug("Kind.Render: rendering event body",
		zap.String("kind", string(k)), zap.Uint64("event_id", e.ID), zap.String("type", string(e.Type)))
	switch k {
	case KindFeishu:
		return json.Marshal(map[string]any{
			"msg_type": "text",
			"content":  map[string]string{"text": plainText(e)},
		})
	case KindSlack:
		return json.Marshal(map[string]any{
			"text": fmt.Sprintf("%s %s", statusIcon(e), plainText(e)),
		})
	case KindWeCom:
		return json.Marshal(map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]string{"content": weComMarkdown(e)},
		})
	case KindDingTalk:
		return json.Marshal(map[string]any{
			"msgtype": "markdown",
			"markdown": map[string]string{
				// DingTalk requires a title even for markdown, and shows it in
				// the notification preview rather than in the message body.
				"title": subject(e),
				"text":  dingTalkMarkdown(e),
			},
		})
	default:
		return json.Marshal(e)
	}
}

// SignedURL returns the URL to POST to, adding whatever authentication the kind
// carries in the query string.
//
// Only DingTalk does this. Its "加签" mode signs the current timestamp with a
// shared secret, and the signature is only valid for an hour on either side, so
// it cannot be computed once and stored — it is recomputed per delivery, which
// is also why a retry an hour later needs a fresh one.
//
// A secret that is empty means the robot is secured by a keyword or by an IP
// allowlist instead, both of which need nothing from us.
func (k Kind) SignedURL(rawURL, secret string, now time.Time) (string, error) {
	if k != KindDingTalk || secret == "" {
		zap.L().Debug("Kind.SignedURL: no signing needed",
			zap.String("kind", string(k)), zap.Bool("has_secret", secret != ""))
		return rawURL, nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		zap.L().Debug("Kind.SignedURL: parsing the webhook URL failed", zap.String("kind", string(k)), zap.Error(err))
		return "", fmt.Errorf("event: parse webhook url: %w", err)
	}
	ms := strconv.FormatInt(now.UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ms + "\n" + secret))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	q := u.Query()
	q.Set("timestamp", ms)
	q.Set("sign", sig)
	u.RawQuery = q.Encode()
	zap.L().Debug("Kind.SignedURL: URL signed", zap.String("kind", string(k)), zap.String("host", u.Host), zap.String("timestamp", ms))
	return u.String(), nil
}

// CheckResponse turns a service's reply into an error.
//
// Feishu, WeCom and DingTalk all answer HTTP 200 for a rejected message and put
// the real outcome in a JSON "errcode"/"code" field. Trusting the status code
// alone reports every one of those as delivered, which is the worst possible
// failure for an alerting path: it is silent, and it is only discovered when an
// outage passes unnoticed.
//
// Slack replies with a bare "ok" body and uses the status code honestly, so
// there is nothing to inspect; generic receivers are whatever the operator
// built, so the status code is all there is.
func (k Kind) CheckResponse(body []byte) error {
	switch k {
	case KindFeishu, KindWeCom, KindDingTalk:
		zap.L().Debug("Kind.CheckResponse: inspecting reply body", zap.String("kind", string(k)), zap.Int("reply_bytes", len(body)))
	default:
		zap.L().Debug("Kind.CheckResponse: kind trusts the status code, body not inspected", zap.String("kind", string(k)))
		return nil
	}
	var r struct {
		// Feishu uses "code" (and "StatusCode" on some older endpoints), WeCom
		// and DingTalk use "errcode". Every one of them means zero is success.
		Code    *int   `json:"code"`
		ErrCode *int   `json:"errcode"`
		Msg     string `json:"msg"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		// A body that is not JSON is not a reply this service produces. Report
		// it rather than assuming success — it is usually a proxy error page.
		zap.L().Debug("Kind.CheckResponse: reply is not JSON", zap.String("kind", string(k)), zap.Error(err))
		return fmt.Errorf("unexpected reply from the webhook: %s", truncate(string(body), 200))
	}
	code := 0
	if r.Code != nil {
		code = *r.Code
	}
	if r.ErrCode != nil {
		code = *r.ErrCode
	}
	if code == 0 {
		zap.L().Debug("Kind.CheckResponse: service accepted the message", zap.String("kind", string(k)))
		return nil
	}
	msg := r.Msg
	if msg == "" {
		msg = r.ErrMsg
	}
	zap.L().Debug("Kind.CheckResponse: service rejected the message",
		zap.String("kind", string(k)), zap.Int("code", code), zap.String("msg", msg))
	return fmt.Errorf("the webhook rejected the message: code %d: %s", code, msg)
}

// subject is the one-line headline for an event.
func subject(e Event) string {
	switch {
	case e.Resource != "" && e.Node != "":
		return fmt.Sprintf("Haify %s: %s on %s", e.Type, e.Resource, e.Node)
	case e.Resource != "":
		return fmt.Sprintf("Haify %s: %s", e.Type, e.Resource)
	case e.Node != "":
		return fmt.Sprintf("Haify %s: %s", e.Type, e.Node)
	default:
		return fmt.Sprintf("Haify %s", e.Type)
	}
}

// statusIcon gives the message a shape someone can read at a glance in a busy
// channel, where the difference between a new outage and one that just cleared
// matters more than any other field.
func statusIcon(e Event) string {
	if e.Status == StatusResolved {
		return "✅"
	}
	switch e.Severity {
	case SeverityCritical:
		return "🔴"
	case SeverityWarning:
		return "🟠"
	default:
		return "🔵"
	}
}

// plainText renders an event for services that take unformatted text.
func plainText(e Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] %s\n%s", strings.ToUpper(string(e.Severity)), statusLabel(e), e.Message)
	for _, line := range detailLines(e) {
		b.WriteString("\n")
		b.WriteString(line)
	}
	return b.String()
}

func weComMarkdown(e Event) string {
	// WeCom supports a small colour vocabulary in markdown and nothing else —
	// no headings, no tables. info/comment/warning are the only three.
	colour := "info"
	switch {
	case e.Status == StatusResolved:
		colour = "info"
	case e.Severity == SeverityCritical:
		colour = "warning"
	case e.Severity == SeverityWarning:
		colour = "comment"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s **%s**\n<font color=\"%s\">%s</font>\n%s",
		statusIcon(e), subject(e), colour, statusLabel(e), e.Message)
	for _, line := range detailLines(e) {
		b.WriteString("\n> ")
		b.WriteString(line)
	}
	return b.String()
}

func dingTalkMarkdown(e Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#### %s %s\n\n**%s**\n\n%s", statusIcon(e), subject(e), statusLabel(e), e.Message)
	for _, line := range detailLines(e) {
		b.WriteString("\n\n- ")
		b.WriteString(line)
	}
	return b.String()
}

// statusLabel says what the event does to a condition, in words rather than in
// the wire value.
func statusLabel(e Event) string {
	switch e.Status {
	case StatusFiring:
		return "FIRING"
	case StatusResolved:
		return "RESOLVED"
	default:
		return "EVENT"
	}
}

// detailLines renders the scoping fields and the event's own details, sorted so
// the same event always reads the same way.
func detailLines(e Event) []string {
	var out []string
	if e.Resource != "" {
		out = append(out, "resource: "+e.Resource)
	}
	if e.Node != "" {
		out = append(out, "node: "+e.Node)
	}
	keys := make([]string, 0, len(e.Details))
	for k := range e.Details {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+": "+e.Details[k])
	}
	if !e.Timestamp.IsZero() {
		out = append(out, "time: "+e.Timestamp.UTC().Format(time.RFC3339))
	}
	return out
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
