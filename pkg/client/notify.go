package client

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// NotifyChannelSpec is one alert delivery channel as a caller supplies it.
//
// Secret is write-only end to end: it is never returned by ListNotifyChannels,
// so an edit that leaves it empty keeps whatever is stored. ClearSecret is the
// only way to remove one.
type NotifyChannelSpec struct {
	Name        string
	Kind        string
	URL         string
	MinSeverity string
	Types       []string
	Headers     map[string]string
	Enabled     bool
	Secret      string
	ClearSecret bool
}

// ListNotifyChannels returns every channel (without secrets) and the message
// formats this controller can render.
func (c *HaifyClient) ListNotifyChannels(ctx context.Context) ([]*haifypb.NotifyChannelInfo, []string, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListNotifyChannels: sending request")
	resp, err := c.client.ListNotifyChannels(ctx, &haifypb.ListNotifyChannelsRequest{})
	if err != nil {
		log.Debug("HaifyClient.ListNotifyChannels: ListNotifyChannels failed", zap.Error(err))
		return nil, nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.ListNotifyChannels: controller reported failure", zap.String("message", resp.Message))
		return nil, resp.Kinds, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.ListNotifyChannels: done", zap.Int("count", len(resp.Channels)))
	return resp.Channels, resp.Kinds, nil
}

// SaveNotifyChannel creates a channel or replaces one with the same name.
func (c *HaifyClient) SaveNotifyChannel(ctx context.Context, spec NotifyChannelSpec) (*haifypb.NotifyChannelInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.SaveNotifyChannel: sending request", zap.String("name", spec.Name), zap.String("kind", spec.Kind),
		zap.Bool("has_url", spec.URL != ""), zap.String("min_severity", spec.MinSeverity), zap.Strings("types", spec.Types),
		zap.Int("headers_count", len(spec.Headers)), zap.Bool("enabled", spec.Enabled), zap.Bool("has_secret", spec.Secret != ""),
		zap.Bool("clear_secret", spec.ClearSecret))
	resp, err := c.client.SaveNotifyChannel(ctx, &haifypb.SaveNotifyChannelRequest{
		Name:        spec.Name,
		Kind:        spec.Kind,
		Url:         spec.URL,
		MinSeverity: spec.MinSeverity,
		Types:       spec.Types,
		Headers:     spec.Headers,
		Enabled:     spec.Enabled,
		Secret:      spec.Secret,
		ClearSecret: spec.ClearSecret,
	})
	if err != nil {
		log.Debug("HaifyClient.SaveNotifyChannel: SaveNotifyChannel failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.SaveNotifyChannel: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.SaveNotifyChannel: done")
	return resp.Channel, nil
}

// DeleteNotifyChannel removes a channel.
func (c *HaifyClient) DeleteNotifyChannel(ctx context.Context, name string) error {
	log := zap.L()
	log.Debug("HaifyClient.DeleteNotifyChannel: sending request", zap.String("name", name))
	resp, err := c.client.DeleteNotifyChannel(ctx, &haifypb.DeleteNotifyChannelRequest{Name: name})
	if err != nil {
		log.Debug("HaifyClient.DeleteNotifyChannel: DeleteNotifyChannel failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.DeleteNotifyChannel: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.DeleteNotifyChannel: done")
	return nil
}

// TestNotifyChannel sends one message to a single channel and reports what the
// far end said. The error carries the service's own rejection text, which is
// the part that says what to fix.
func (c *HaifyClient) TestNotifyChannel(ctx context.Context, name string) (string, error) {
	log := zap.L()
	log.Debug("HaifyClient.TestNotifyChannel: sending request", zap.String("name", name))
	resp, err := c.client.TestNotifyChannel(ctx, &haifypb.TestNotifyChannelRequest{Name: name})
	if err != nil {
		log.Debug("HaifyClient.TestNotifyChannel: TestNotifyChannel failed", zap.Error(err))
		return "", err
	}
	if !resp.Success {
		log.Debug("HaifyClient.TestNotifyChannel: controller reported failure", zap.String("message", resp.Message))
		return "", fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.TestNotifyChannel: done")
	return resp.Message, nil
}
