package silgotel

import (
	"context"
	"errors"

	"github.com/go-playground/validator/v10"
)

func Validate(object interface{}) error {
	v := validator.New()
	err := v.Struct(object)

	return err
}

type Client struct {
	OTLPBaseURL string `json:"otlpBaseURL" validate:"required"`
	ServiceName string `json:"serviceName" validate:"required"`
	Environment string `json:"environment" validate:"required"`
	Version     string `json:"version" validate:"required"`
}

func NewOtelSDK(ctx context.Context, client *Client) (*Client, error) {
	err := Validate(client)
	if err != nil {
		return nil, err
	}

	c := &Client{
		OTLPBaseURL: client.OTLPBaseURL,
		ServiceName: client.ServiceName,
		Environment: client.Environment,
		Version:     client.Version,
	}

	otelShutdownFn, err := c.setupOtelSDK(ctx)
	if err != nil {
		return nil, err
	}

	defer func() {
		_ = errors.Join(err, otelShutdownFn(ctx))
	}()

	return c, nil
}
