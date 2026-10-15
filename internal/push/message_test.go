package push_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/backplane-labs/backplane-courier/internal/push"
)

func TestValidate(t *testing.T) {
	t.Parallel()

	if err := (push.Message{Token: "a", Title: "Hi"}).Validate(); err != nil {
		t.Fatal(err)
	}

	// Data-only and empty pushes are FCM's to judge: a silent wake-up is valid.
	if err := (push.Message{Token: "a", Data: map[string]string{"k": "v"}}).Validate(); err != nil {
		t.Fatal(err)
	}

	bad := map[string]push.Message{
		"none":      {Title: "x"},
		"two":       {Token: "a", Topic: "t"},
		"token":     {Token: "a b"},
		"topic":     {Topic: "a b"},
		"condition": {Condition: "x"},
		"reserved":  {Topic: "t", Data: map[string]string{"google.x": "1"}},
		"priority":  {Topic: "t", Priority: "urgent"},
		"ttl":       {Topic: "t", TTLSeconds: -1},
		"big":       {Topic: "t", Body: strings.Repeat("x", 6000)},
	}
	for name, m := range bad {
		if err := m.Validate(); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

// Raw blocks reach FCM merged over the modeled fields.
func TestBuild(t *testing.T) {
	t.Parallel()

	m := push.Message{
		Token: "tok", Title: "Hi", Data: map[string]string{"k": "v"}, Priority: "high", TTLSeconds: 60, CollapseKey: "c",
		APNS:       map[string]any{"headers": map[string]any{"apns-push-type": "alert"}, "payload": map[string]any{"aps": map[string]any{"mutable-content": 1}}},
		Android:    map[string]any{"direct_boot_ok": true},
		FCMOptions: map[string]any{"analytics_label": "l"}, ValidateOnly: true,
	}

	body, err := push.Build(m)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}

	msg := got["message"].(map[string]any)
	apns := msg["apns"].(map[string]any)
	headers := apns["headers"].(map[string]any)
	android := msg["android"].(map[string]any)

	if got["validate_only"] != true || msg["token"] != "tok" || headers["apns-priority"] != "10" || headers["apns-push-type"] != "alert" ||
		headers["apns-collapse-id"] != "c" || apns["payload"].(map[string]any)["aps"].(map[string]any)["mutable-content"] != 1.0 ||
		android["priority"] != "HIGH" || android["ttl"] != "60s" || android["direct_boot_ok"] != true ||
		msg["fcm_options"].(map[string]any)["analytics_label"] != "l" {
		t.Fatalf("built %s", body)
	}

	topic, _ := push.Build(push.Message{Topic: "/topics/news", Data: map[string]string{"a": "b"}})
	if !strings.Contains(string(topic), `"topic":"news"`) || strings.Contains(string(topic), "notification") {
		t.Fatalf("topic %s", topic)
	}
}
