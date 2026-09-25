package silgotel

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
)

func TestClient_newResource(t *testing.T) {
	tests := []struct {
		name   string
		client Client
		want   map[attribute.Key]string
	}{
		{
			name: "service attributes are set",
			client: Client{
				ServiceName: "clinical",
				Environment: "staging",
				Version:     "v1.0.0",
			},
			want: map[attribute.Key]string{
				"service.name":                "clinical",
				"service.version":             "v1.0.0",
				"deployment.environment.name": "staging",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := tt.client.newResource(context.Background())
			if err != nil {
				t.Fatalf("newResource() error = %v", err)
			}

			if got, want := res.SchemaURL(), resource.Default().SchemaURL(); got != want {
				t.Errorf("SchemaURL() = %q, want %q", got, want)
			}

			for key, want := range tt.want {
				got, ok := res.Set().Value(key)
				if !ok || got.AsString() != want {
					t.Errorf("attribute %q = %q, want %q", key, got.AsString(), want)
				}
			}
		})
	}
}
