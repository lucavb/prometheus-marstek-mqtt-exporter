package mqtt

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	"github.com/lucavb/prometheus-marstek-mqtt-exporter/config"
)

// Hame protocol topic templates.
const (
	commandTopicTpl = "hame_energy/%s/App/%s/ctrl"
	statusTopicTpl  = "hame_energy/%s/device/%s/ctrl"
)

type MessageHandler func(payload string)

type Client struct {
	client       paho.Client
	commandTopic string
	statusTopic  string
}

func New(cfg *config.Config) *Client {
	commandTopic := fmt.Sprintf(commandTopicTpl, cfg.DeviceType, cfg.DeviceID)
	statusTopic := fmt.Sprintf(statusTopicTpl, cfg.DeviceType, cfg.DeviceID)

	broker := fmt.Sprintf("tcp://%s:%d", cfg.MQTTHost, cfg.MQTTPort)

	opts := paho.NewClientOptions().
		AddBroker(broker).
		SetClientID(cfg.MQTTClientID).
		SetKeepAlive(30 * time.Second).
		SetCleanSession(true).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		SetConnectionLostHandler(func(_ paho.Client, err error) {
			slog.Warn("mqtt connection lost", "error", err)
		}).
		SetOnConnectHandler(func(_ paho.Client) {
			slog.Info("mqtt connected", "broker", broker)
		}).
		SetReconnectingHandler(func(_ paho.Client, _ *paho.ClientOptions) {
			slog.Info("mqtt reconnecting", "broker", broker)
		})

	if cfg.MQTTUsername != "" {
		opts.SetUsername(cfg.MQTTUsername)
	}
	if cfg.MQTTPassword != "" {
		opts.SetPassword(cfg.MQTTPassword)
	}

	return &Client{
		client:       paho.NewClient(opts),
		commandTopic: commandTopic,
		statusTopic:  statusTopic,
	}
}

func (c *Client) Connect(ctx context.Context) error {
	token := c.client.Connect()
	select {
	case <-token.Done():
		if err := token.Error(); err != nil {
			return fmt.Errorf("mqtt connect: %w", err)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) Subscribe(handler MessageHandler) error {
	token := c.client.Subscribe(c.statusTopic, 0, func(_ paho.Client, msg paho.Message) {
		slog.Debug("mqtt message received", "topic", msg.Topic(), "payload", string(msg.Payload()))
		handler(string(msg.Payload()))
	})
	token.Wait()
	if err := token.Error(); err != nil {
		return fmt.Errorf("mqtt subscribe to %s: %w", c.statusTopic, err)
	}
	slog.Info("mqtt subscribed", "topic", c.statusTopic)
	return nil
}

func (c *Client) Poll() error {
	token := c.client.Publish(c.commandTopic, 0, false, "cd=1")
	token.Wait()
	if err := token.Error(); err != nil {
		return fmt.Errorf("mqtt publish to %s: %w", c.commandTopic, err)
	}
	slog.Debug("mqtt poll sent", "topic", c.commandTopic)
	return nil
}

func (c *Client) Close() {
	c.client.Disconnect(2000)
	slog.Info("mqtt disconnected")
}
