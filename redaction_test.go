package otel

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// The cases below are the shared contract, and the same table is asserted in
// otel-instrumentation-next and Netdb.Observability. A name that redacts in one
// language must redact in all three.
func TestShouldRedact_SharedContract(t *testing.T) {
	redacted := []string{
		"password", "Password", "PASSWORD", "passwd", "userPassword",
		"secret", "clientSecret", "client_secret", "sessionSecret",
		"token", "accessToken", "access_token", "refresh_token", "Refresh-Token",
		"apikey", "apiKey", "api_key", "X-API-KEY", "Api.Key", "x-api-key",
		"authorization", "Authorization", "proxy-authorization",
		"cookie", "Cookie", "set-cookie",
		"credential", "db_credential", "Credentials",
	}
	for _, name := range redacted {
		if !ShouldRedact(name) {
			t.Errorf("ShouldRedact(%q) = false, want true", name)
		}
	}

	kept := []string{
		"username", "user_id", "email", "duration_ms", "trace_id", "span_id",
		"request.id", "service.name", "status_code", "message",
	}
	for _, name := range kept {
		if ShouldRedact(name) {
			t.Errorf("ShouldRedact(%q) = true, want false", name)
		}
	}
}

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
	APIKey   string `json:"api_key"`
	Ignored  string `json:"-"`
	// A secret-named field behind an innocuous json tag. The Go field name is
	// matched too, or renaming the tag would be enough to defeat redaction.
	SessionToken string `json:"sid"`
}

type account struct {
	ID    int          `json:"id"`
	Creds *credentials `json:"creds"`
}

// Rule 4 of the contract: a secret nested inside a logged struct must be
// censored, not passed through because the top-level field name looked clean.
func TestRedaction_NestedStructFields(t *testing.T) {
	var buf bytes.Buffer
	captureLogger(&buf).Info("login", zap.Any("account", account{
		ID: 7,
		Creds: &credentials{
			Username:     "yannick",
			Password:     "hunter2",
			APIKey:       "ak-1",
			Ignored:      "not-serialised",
			SessionToken: "tagged-secret",
		},
	}))

	out := buf.String()
	for _, leak := range []string{"hunter2", "ak-1", "tagged-secret"} {
		if strings.Contains(out, leak) {
			t.Errorf("nested secret %q leaked: %s", leak, out)
		}
	}
	if !strings.Contains(out, "yannick") {
		t.Errorf("nested non-secret was dropped: %s", out)
	}
	if strings.Count(out, redactedPlaceholder) != 3 {
		t.Errorf("expected 3 redactions, got %d: %s", strings.Count(out, redactedPlaceholder), out)
	}
}

// json:"-" fields are dropped by the encoder either way, but a struct rebuilt
// during redaction must not resurrect them.
func TestRedaction_RewrittenStructDropsIgnoredFields(t *testing.T) {
	var buf bytes.Buffer
	captureLogger(&buf).Info("m", zap.Any("creds", credentials{Ignored: "not-serialised"}))

	if strings.Contains(buf.String(), "not-serialised") {
		t.Errorf(`json:"-" field was resurrected by the rewrite: %s`, buf.String())
	}
}

func TestRedaction_NestedMapKeys(t *testing.T) {
	var buf bytes.Buffer
	captureLogger(&buf).Info("m", zap.Any("payload", map[string]any{
		"user": map[string]any{
			"name":         "yannick",
			"sessionToken": "st-1",
			"X-Api-Key":    "ak-1",
			"nested": map[string]any{
				"authorization": "Bearer abc",
			},
		},
	}))

	out := buf.String()
	for _, leak := range []string{"st-1", "ak-1", "Bearer abc"} {
		if strings.Contains(out, leak) {
			t.Errorf("nested secret %q leaked: %s", leak, out)
		}
	}
	if !strings.Contains(out, "yannick") {
		t.Errorf("nested non-secret was dropped: %s", out)
	}
}

func TestRedaction_SecretsInsideSlices(t *testing.T) {
	var buf bytes.Buffer
	captureLogger(&buf).Info("m", zap.Any("users", []map[string]string{
		{"name": "a", "token": "tok-a"},
		{"name": "b", "token": "tok-b"},
	}))

	out := buf.String()
	if strings.Contains(out, "tok-a") || strings.Contains(out, "tok-b") {
		t.Errorf("secret inside a slice leaked: %s", out)
	}
	if !strings.Contains(out, `"a"`) || !strings.Contains(out, `"b"`) {
		t.Errorf("non-secret slice entries were dropped: %s", out)
	}
}

// A clean value must survive untouched, keeping its json tags and field order -
// redaction that reshapes every log line is redaction people turn off.
func TestRedaction_LeavesCleanValuesUntouched(t *testing.T) {
	var buf bytes.Buffer
	captureLogger(&buf).Info("m", zap.Any("account", map[string]any{
		"id":    7,
		"email": "a@b.c",
	}))

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("not json: %v", err)
	}
	acct, ok := line["account"].(map[string]any)
	if !ok {
		t.Fatalf("account field missing or reshaped: %s", buf.String())
	}
	if acct["email"] != "a@b.c" || acct["id"] != float64(7) {
		t.Errorf("clean value was altered: %v", acct)
	}
}

// A cyclic structure must terminate rather than hang the logger.
func TestRedaction_CyclicValueTerminates(t *testing.T) {
	type node struct {
		Name  string
		Token string
		Next  *node
	}
	a := &node{Name: "a", Token: "tok-1"}
	b := &node{Name: "b", Token: "tok-2", Next: a}
	a.Next = b

	var buf bytes.Buffer
	captureLogger(&buf).Info("m", zap.Any("graph", a))

	if strings.Contains(buf.String(), "tok-1") {
		t.Errorf("secret leaked from cyclic value: %s", buf.String())
	}
}

type secretObject struct{}

func (secretObject) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddString("username", "yannick")
	enc.AddString("password", "hunter2")
	enc.AddInt("attempts", 3)
	return enc.AddObject("inner", zapcore.ObjectMarshalerFunc(func(inner zapcore.ObjectEncoder) error {
		inner.AddString("refreshToken", "rt-1")
		return nil
	}))
}

// zap.Object bypasses reflection entirely, so it needs the encoder wrapper.
func TestRedaction_ObjectMarshaler(t *testing.T) {
	var buf bytes.Buffer
	captureLogger(&buf).Info("m", zap.Object("session", secretObject{}))

	out := buf.String()
	for _, leak := range []string{"hunter2", "rt-1"} {
		if strings.Contains(out, leak) {
			t.Errorf("secret %q leaked through zap.Object: %s", leak, out)
		}
	}
	if !strings.Contains(out, "yannick") {
		t.Errorf("non-secret was dropped: %s", out)
	}
}

func TestRedaction_ArrayMarshaler(t *testing.T) {
	var buf bytes.Buffer
	captureLogger(&buf).Info("m", zap.Array("sessions",
		zapcore.ArrayMarshalerFunc(func(arr zapcore.ArrayEncoder) error {
			return arr.AppendObject(secretObject{})
		})))

	if strings.Contains(buf.String(), "hunter2") {
		t.Errorf("secret leaked through zap.Array: %s", buf.String())
	}
}

// zap.Namespace has no closing call, so a secret-named namespace has to censor
// everything that follows it.
func TestRedaction_SecretNamespaceCensorsEverythingAfterIt(t *testing.T) {
	var buf bytes.Buffer
	captureLogger(&buf).Info("m",
		zap.String("username", "yannick"),
		zap.Namespace("credentials"),
		zap.String("kind", "basic"),
		zap.String("value", "hunter2"),
	)

	out := buf.String()
	if strings.Contains(out, "hunter2") || strings.Contains(out, "basic") {
		t.Errorf("fields inside a secret namespace leaked: %s", out)
	}
	if !strings.Contains(out, "yannick") {
		t.Errorf("field before the namespace was dropped: %s", out)
	}
}
