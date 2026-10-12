package backup

import (
	"fmt"
	"net"
	"strings"

	"go.uber.org/zap"
)

// Rendering a target into the single-remote rclone config that Prepare stages
// on the node. The rendered text holds the target's credentials, so nothing
// here logs it: only the target's name, kind and which credentials are set.

// renderRcloneConfig produces the single-remote rclone config for a target.
func renderRcloneConfig(t TargetSpec) (string, error) {
	log := zap.L()
	log.Debug("renderRcloneConfig: rendering", zap.String("target", t.Name), zap.String("kind", string(t.Kind)),
		zap.Bool("has_user", t.User != ""), zap.Bool("has_secret", t.Secret != ""))
	if err := checkConfigValues(t); err != nil {
		log.Debug("renderRcloneConfig: rejected value", zap.String("target", t.Name), zap.Error(err))
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[%s]\n", remoteName)
	switch t.Kind {
	case KindS3:
		region := t.Region
		if region == "" {
			// Many S3 clones reject an empty region outright, and AWS treats
			// us-east-1 as the neutral default, so this is the safe filler
			// rather than a guess about the operator's deployment.
			region = "us-east-1"
		}
		log.Debug("renderRcloneConfig: s3 remote", zap.String("target", t.Name), zap.String("region", region),
			zap.String("endpoint_host", endpointHost(t.Endpoint)))
		fmt.Fprintf(&b, "type = s3\nprovider = Other\nenv_auth = false\n")
		fmt.Fprintf(&b, "access_key_id = %s\nsecret_access_key = %s\nregion = %s\n", t.User, t.Secret, region)
		if t.Endpoint != "" {
			fmt.Fprintf(&b, "endpoint = %s\n", t.Endpoint)
		}
		// A backup user usually has PutObject on one bucket and nothing else;
		// rclone's default bucket existence check would fail on the missing
		// permission before a single byte moved.
		fmt.Fprintf(&b, "no_check_bucket = true\n")
	case KindSMB:
		pass, err := configPassword(t)
		if err != nil {
			log.Debug("renderRcloneConfig: smb password unusable", zap.String("target", t.Name), zap.Error(err))
			return "", err
		}
		host, port := splitSMBHost(t.Host)
		log.Debug("renderRcloneConfig: smb remote", zap.String("target", t.Name), zap.String("host", host),
			zap.String("port", port), zap.Bool("has_password", pass != ""))
		fmt.Fprintf(&b, "type = smb\nhost = %s\n", host)
		if port != "" {
			fmt.Fprintf(&b, "port = %s\n", port)
		}
		if t.User != "" {
			fmt.Fprintf(&b, "user = %s\n", t.User)
		}
		if pass != "" {
			fmt.Fprintf(&b, "pass = %s\n", pass)
		}
	case KindWebDAV:
		pass, err := configPassword(t)
		if err != nil {
			log.Debug("renderRcloneConfig: webdav password unusable", zap.String("target", t.Name), zap.Error(err))
			return "", err
		}
		log.Debug("renderRcloneConfig: webdav remote", zap.String("target", t.Name),
			zap.String("endpoint_host", endpointHost(t.Endpoint)), zap.Bool("has_password", pass != ""))
		fmt.Fprintf(&b, "type = webdav\nurl = %s\nvendor = other\n", t.Endpoint)
		if t.User != "" {
			fmt.Fprintf(&b, "user = %s\n", t.User)
		}
		if pass != "" {
			fmt.Fprintf(&b, "pass = %s\n", pass)
		}
	default:
		log.Debug("renderRcloneConfig: unknown kind", zap.String("target", t.Name), zap.String("kind", string(t.Kind)))
		return "", fmt.Errorf("backup: unknown target kind %q", t.Kind)
	}
	return b.String(), nil
}

// splitSMBHost separates an optional port from an SMB host.
//
// rclone's smb backend appends its own ":445" to whatever `host` holds, so a
// host:port left intact dials "host:port:445" and fails with "too many colons
// in address" — a message that names neither the target nor the setting behind
// it. The port has to travel in its own config key.
//
// A bracketed IPv6 literal with no port ("[::1]") makes SplitHostPort report a
// missing port; that is precisely the signal that there is nothing to split, so
// the value goes through untouched and rclone appends the default itself.
func splitSMBHost(h string) (host, port string) {
	h = strings.TrimSpace(h)
	if !strings.Contains(h, ":") {
		return h, ""
	}
	host, port, err := net.SplitHostPort(h)
	if err != nil {
		zap.L().Debug("splitSMBHost: no port to split, kept as is", zap.String("host", h), zap.Error(err))
		return h, ""
	}
	zap.L().Debug("splitSMBHost: port split off", zap.String("host", host), zap.String("port", port))
	return host, port
}

// configPassword returns the obscured password for an SMB/WebDAV target.
func configPassword(t TargetSpec) (string, error) {
	log := zap.L()
	if t.Secret == "" {
		log.Debug("configPassword: no password", zap.String("target", t.Name))
		return "", nil
	}
	if t.SecretIsObscured {
		log.Debug("configPassword: secret already obscured, checking it decodes", zap.String("target", t.Name))
		// Trust it, but confirm it decodes: an operator who pasted a plain
		// password into the "already obscured" slot would otherwise get an
		// authentication failure at backup time with nothing pointing at the
		// cause.
		if _, err := Reveal(t.Secret); err != nil {
			log.Debug("configPassword: obscured secret does not decode", zap.String("target", t.Name), zap.Error(err))
			return "", err
		}
		return t.Secret, nil
	}
	log.Debug("configPassword: obscuring plain password", zap.String("target", t.Name))
	return Obscure(t.Secret)
}

// checkConfigValues rejects values that would corrupt the rendered config.
// rclone's config is line-oriented INI with unquoted values, so a newline in
// any field would silently inject or truncate settings.
func checkConfigValues(t TargetSpec) error {
	for name, v := range map[string]string{
		"endpoint": t.Endpoint, "region": t.Region, "bucket": t.Bucket,
		"host": t.Host, "share": t.Share, "user": t.User, "secret": t.Secret,
		"prefix": t.Prefix,
	} {
		if strings.ContainsAny(v, "\r\n") {
			zap.L().Debug("checkConfigValues: newline in field", zap.String("target", t.Name), zap.String("field", name))
			return fmt.Errorf("backup: target field %q must not contain a newline", name)
		}
	}
	return nil
}

// endpointHost is the host part of an S3 or WebDAV endpoint, for traces: the
// scheme, path and any user:password@ an operator wrote into the URL are
// dropped, so a credential embedded in the endpoint never reaches the log.
func endpointHost(endpoint string) string {
	if _, rest, ok := strings.Cut(endpoint, "://"); ok {
		endpoint = rest
	}
	if i := strings.IndexAny(endpoint, "/?#"); i >= 0 {
		endpoint = endpoint[:i]
	}
	if i := strings.LastIndex(endpoint, "@"); i >= 0 {
		endpoint = endpoint[i+1:]
	}
	return endpoint
}
