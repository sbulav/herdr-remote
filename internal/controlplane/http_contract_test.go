package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dcolinmorgan/herdr-remote/internal/protocol"
	"github.com/dcolinmorgan/herdr-remote/internal/push"
	"github.com/dcolinmorgan/herdr-remote/internal/store"
)

type httpContractEndpoint struct {
	Method         string            `json:"method"`
	Path           string            `json:"path"`
	OriginRequired bool              `json:"origin_required"`
	CSRFRequired   bool              `json:"csrf_required"`
	Request        *string           `json:"request"`
	Responses      map[string]string `json:"responses"`
	Status         int               `json:"-"`
	ResponseSchema string            `json:"-"`
}

type httpContractFixture struct {
	Version int `json:"version"`
	Cases   map[string]struct {
		Request  any `json:"request"`
		Status   int `json:"status"`
		Response any `json:"response"`
	} `json:"cases"`
}
type loadedHTTPContract struct {
	Endpoints map[string]httpContractEndpoint
	Schema    map[string]any
}

func TestBrowserHTTPContractEndToEnd(t *testing.T) {
	contract := loadHTTPContract(t)
	base, hub, st := testServer(t)
	defer st.Close()
	sender := recordingPushSender{sent: make(chan push.Event, 8)}
	cfg := base.cfg
	cfg.Push = &push.Service{Store: st, Sender: sender, MaxAttempts: 1}
	cfg.VAPIDPublicKey = testVAPIDPublicKey(t)
	cfg.OperatorSubject = "operator"
	server, err := NewServer(cfg, hub)
	if err != nil {
		t.Fatal(err)
	}
	handler := server.BrowserHandler()

	sessionEndpoint := contract.Endpoints["session"]
	sessionResponse := contractRequest(t, handler, sessionEndpoint, "", nil, "", "")
	assertStatus(t, sessionResponse, sessionEndpoint.Status)
	cookies := sessionResponse.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "__Host-herdr_session" || !cookies[0].HttpOnly || !cookies[0].Secure {
		t.Fatalf("session cookies = %#v", cookies)
	}
	cookie := cookies[0]
	var sessionBody map[string]any
	decodeExactJSON(t, sessionResponse, []string{"authenticated", "operator", "push_public_key", "logout_url"}, &sessionBody)
	assertJSONSchema(t, contract.Schema, sessionEndpoint.ResponseSchema, sessionBody)
	operator, ok := sessionBody["operator"].(map[string]any)
	if sessionBody["authenticated"] != true || !ok || len(operator) != 1 || operator["display_name"] != "operator" || sessionBody["push_public_key"] != cfg.VAPIDPublicKey || sessionBody["logout_url"] != cfg.UpstreamLogoutURL {
		t.Fatalf("session body = %#v", sessionBody)
	}
	if strings.Contains(sessionResponse.Body.String(), "issuer") || strings.Contains(sessionResponse.Body.String(), "audience") || strings.Contains(sessionResponse.Body.String(), "mfa") {
		t.Fatalf("session leaked proxy claims: %s", sessionResponse.Body.String())
	}

	csrfEndpoint := contract.Endpoints["csrf"]
	csrfResponse := contractRequest(t, handler, csrfEndpoint, "", cookie, "", "")
	assertStatus(t, csrfResponse, csrfEndpoint.Status)
	if len(csrfResponse.Result().Cookies()) != 0 {
		t.Fatal("CSRF endpoint created a cookie")
	}
	var csrfBody map[string]any
	decodeExactJSON(t, csrfResponse, []string{"token"}, &csrfBody)
	assertJSONSchema(t, contract.Schema, csrfEndpoint.ResponseSchema, csrfBody)
	csrf, ok := csrfBody["token"].(string)
	if !ok || csrf == "" {
		t.Fatalf("CSRF body = %#v", csrfBody)
	}
	enrollmentEndpoint := contract.Endpoints["enrollment_create"]
	enrollmentBody := `{"display_name":"workstation"}`
	assertJSONBodySchema(t, contract.Schema, *enrollmentEndpoint.Request, enrollmentBody)
	enrollmentResponse := contractRequest(t, handler, enrollmentEndpoint, enrollmentBody, cookie, csrf, cfg.Origin)
	assertStatus(t, enrollmentResponse, enrollmentEndpoint.Status)
	var enrollmentResult map[string]any
	decodeExactJSON(t, enrollmentResponse, []string{"token", "host_id", "expires_at"}, &enrollmentResult)
	assertJSONSchema(t, contract.Schema, enrollmentEndpoint.ResponseSchema, enrollmentResult)

	actionID := "019f64ca-3000-7000-8000-000000000105"
	if err := st.BeginAction(context.Background(), store.ActionIntent{ActionID: actionID, OperationType: "agent.read", Issuer: "issuer", Subject: "operator", HostID: "host", InstanceID: "default", TerminalID: "term"}); err != nil {
		t.Fatal(err)
	}
	code := "CONNECTION_LOST"
	if err := st.Complete(context.Background(), actionID, "failed", &code, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	actionEndpoint := contract.Endpoints["action_status"]
	actionEndpoint.Path = strings.Replace(actionEndpoint.Path, "{action_id}", actionID, 1)
	actionResponse := contractRequest(t, handler, actionEndpoint, "", cookie, "", "")
	assertStatus(t, actionResponse, actionEndpoint.Status)
	var actionResult map[string]any
	decodeExactJSON(t, actionResponse, []string{"action_id", "operation_type", "status", "code", "requested_at", "completed_at"}, &actionResult)
	assertJSONSchema(t, contract.Schema, actionEndpoint.ResponseSchema, actionResult)

	otherEndpoint := "https://push.example/other-device"
	if err := st.UpsertPush(context.Background(), store.PushSubscription{Subject: "operator", Endpoint: otherEndpoint, P256DH: "other-key", Auth: "other-auth"}); err != nil {
		t.Fatal(err)
	}
	localEndpoint := "https://push.example/current-device"
	registerEndpoint := contract.Endpoints["push_register"]
	registerBody := `{"endpoint":"` + localEndpoint + `","keys":{"p256dh":"local-key","auth":"local-auth"}}`
	assertJSONBodySchema(t, contract.Schema, *registerEndpoint.Request, registerBody)
	missingOrigin := contractRequest(t, handler, registerEndpoint, registerBody, cookie, csrf, "")
	assertStatus(t, missingOrigin, http.StatusForbidden)
	badCSRF := contractRequest(t, handler, registerEndpoint, registerBody, cookie, "wrong", cfg.Origin)
	assertStatus(t, badCSRF, http.StatusForbidden)
	registerResponse := contractRequest(t, handler, registerEndpoint, registerBody, cookie, csrf, cfg.Origin)
	assertStatus(t, registerResponse, registerEndpoint.Status)
	if registerResponse.Body.Len() != 0 {
		t.Fatalf("register body = %q", registerResponse.Body.String())
	}
	assertJSONSchema(t, contract.Schema, registerEndpoint.ResponseSchema, nil)

	reconcileEndpoint := contract.Endpoints["push_reconcile"]
	reconcileBodyJSON := `{"endpoint":"` + localEndpoint + `"}`
	assertJSONBodySchema(t, contract.Schema, *reconcileEndpoint.Request, reconcileBodyJSON)
	reconcileResponse := contractRequest(t, handler, reconcileEndpoint, reconcileBodyJSON, cookie, csrf, cfg.Origin)
	assertStatus(t, reconcileResponse, reconcileEndpoint.Status)
	var reconcileBody map[string]any
	decodeExactJSON(t, reconcileResponse, []string{"subscribed"}, &reconcileBody)
	assertJSONSchema(t, contract.Schema, reconcileEndpoint.ResponseSchema, reconcileBody)
	if reconcileBody["subscribed"] != true || strings.Contains(reconcileResponse.Body.String(), otherEndpoint) || strings.Contains(reconcileResponse.Body.String(), "key") {
		t.Fatalf("unsafe reconciliation response = %s", reconcileResponse.Body.String())
	}
	missingResponse := contractRequest(t, handler, reconcileEndpoint, `{"endpoint":"https://push.example/not-this-device"}`, cookie, csrf, cfg.Origin)
	var missingBody map[string]any
	decodeExactJSON(t, missingResponse, []string{"subscribed"}, &missingBody)
	if missingBody["subscribed"] != false {
		t.Fatalf("missing reconciliation = %#v", missingBody)
	}
	replaceEndpoint := contract.Endpoints["push_replace"]
	rotatedEndpoint := "https://push.example/rotated-device"
	replaceBody := `{"source_endpoints":["` + localEndpoint + `"],"subscription":{"endpoint":"` + rotatedEndpoint + `","keys":{"p256dh":"rotated-key","auth":"rotated-auth"}}}`
	assertJSONBodySchema(t, contract.Schema, *replaceEndpoint.Request, replaceBody)
	replaceResponse := contractRequest(t, handler, replaceEndpoint, replaceBody, cookie, csrf, cfg.Origin)
	assertStatus(t, replaceResponse, replaceEndpoint.Status)
	assertJSONSchema(t, contract.Schema, replaceEndpoint.ResponseSchema, nil)
	retryResponse := contractRequest(t, handler, replaceEndpoint, replaceBody, cookie, csrf, cfg.Origin)
	assertStatus(t, retryResponse, replaceEndpoint.Status)
	missingReplaceBody := `{"source_endpoints":["https://push.example/removed-by-cleanup"],"subscription":{"endpoint":"https://push.example/current-after-cleanup","keys":{"p256dh":"key","auth":"auth"}}}`
	missingReplaceResponse := contractRequest(t, handler, replaceEndpoint, missingReplaceBody, cookie, csrf, cfg.Origin)
	assertStatus(t, missingReplaceResponse, http.StatusNotFound)
	if missingReplaceResponse.Body.String() != "missing\n" {
		t.Fatalf("missing replacement response leaked detail: %q", missingReplaceResponse.Body.String())
	}
	localEndpoint = rotatedEndpoint

	testEndpoint := contract.Endpoints["push_test_enabled"]
	assertJSONBodySchema(t, contract.Schema, *testEndpoint.Request, `{}`)
	testResponse := contractRequest(t, handler, testEndpoint, `{}`, cookie, csrf, cfg.Origin)
	assertStatus(t, testResponse, testEndpoint.Status)
	var testBody map[string]any
	decodeExactJSON(t, testResponse, []string{"enabled"}, &testBody)
	assertJSONSchema(t, contract.Schema, testEndpoint.ResponseSchema, testBody)
	if testBody["enabled"] != true {
		t.Fatalf("test response = %#v", testBody)
	}
	select {
	case event := <-sender.sent:
		if event.Kind != "test" || event.EventID == "" {
			t.Fatalf("test push event = %#v", event)
		}
	default:
		t.Fatal("test push was not sent through push service")
	}

	deleteEndpoint := contract.Endpoints["push_delete"]
	foreignDeleteEndpoint := "https://push.example/foreign-delete"
	if err := st.UpsertPush(context.Background(), store.PushSubscription{Subject: "other", Endpoint: foreignDeleteEndpoint, P256DH: "foreign-key", Auth: "foreign-auth"}); err != nil {
		t.Fatal(err)
	}
	deleteBody := `{"endpoints":["` + localEndpoint + `","` + foreignDeleteEndpoint + `"]}`
	assertJSONBodySchema(t, contract.Schema, *deleteEndpoint.Request, deleteBody)
	deleteResponse := contractRequest(t, handler, deleteEndpoint, deleteBody, cookie, csrf, cfg.Origin)
	assertStatus(t, deleteResponse, deleteEndpoint.Status)
	assertJSONSchema(t, contract.Schema, deleteEndpoint.ResponseSchema, nil)
	localExists, err := st.HasPushSubscription(context.Background(), "operator", localEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	otherExists, err := st.HasPushSubscription(context.Background(), "operator", otherEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	foreignExists, err := st.HasPushSubscription(context.Background(), "other", foreignDeleteEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	if localExists || !otherExists || !foreignExists {
		t.Fatalf("device-scoped delete local=%t other=%t foreign=%t", localExists, otherExists, foreignExists)
	}
	hostEndpoint := contract.Endpoints["host_credential_delete"]
	hostEndpoint.Path = strings.Replace(hostEndpoint.Path, "{host_id}", enrollmentResult["host_id"].(string), 1)
	hostResponse := contractRequest(t, handler, hostEndpoint, "", cookie, csrf, cfg.Origin)
	assertStatus(t, hostResponse, hostEndpoint.Status)
	assertJSONSchema(t, contract.Schema, hostEndpoint.ResponseSchema, nil)

	logoutEndpoint := contract.Endpoints["logout"]
	assertJSONBodySchema(t, contract.Schema, *logoutEndpoint.Request, `{}`)
	logoutResponse := contractRequest(t, handler, logoutEndpoint, `{}`, cookie, csrf, cfg.Origin)
	assertStatus(t, logoutResponse, logoutEndpoint.Status)
	var logoutBody map[string]any
	decodeExactJSON(t, logoutResponse, []string{"logout_url"}, &logoutBody)
	assertJSONSchema(t, contract.Schema, logoutEndpoint.ResponseSchema, logoutBody)
	if logoutBody["logout_url"] != cfg.UpstreamLogoutURL {
		t.Fatalf("logout URL = %#v", logoutBody["logout_url"])
	}
	cleared := logoutResponse.Result().Cookies()
	if len(cleared) != 1 || cleared[0].Name != "__Host-herdr_session" || cleared[0].MaxAge >= 0 {
		t.Fatalf("logout cookies = %#v", cleared)
	}
	afterLogout := contractRequest(t, handler, csrfEndpoint, "", cookie, "", "")
	assertStatus(t, afterLogout, http.StatusUnauthorized)
}

func TestBrowserHTTPContractReportsPushDisabled(t *testing.T) {
	contract := loadHTTPContract(t)
	server, _, st := testServer(t)
	defer st.Close()
	handler := server.BrowserHandler()
	session := contractRequest(t, handler, contract.Endpoints["session"], "", nil, "", "")
	cookie := session.Result().Cookies()[0]
	var sessionBody map[string]any
	decodeExactJSON(t, session, []string{"authenticated", "operator", "push_public_key", "logout_url"}, &sessionBody)
	assertJSONSchema(t, contract.Schema, contract.Endpoints["session"].ResponseSchema, sessionBody)
	if sessionBody["push_public_key"] != nil {
		t.Fatalf("disabled session key = %#v", sessionBody["push_public_key"])
	}
	csrfResponse := contractRequest(t, handler, contract.Endpoints["csrf"], "", cookie, "", "")
	var csrfBody map[string]any
	decodeExactJSON(t, csrfResponse, []string{"token"}, &csrfBody)
	disabled := contract.Endpoints["push_test_disabled"]
	assertJSONBodySchema(t, contract.Schema, *disabled.Request, `{}`)
	response := contractRequest(t, handler, disabled, `{}`, cookie, csrfBody["token"].(string), server.cfg.Origin)
	assertStatus(t, response, disabled.Status)
	var body map[string]any
	decodeExactJSON(t, response, []string{"enabled"}, &body)
	assertJSONSchema(t, contract.Schema, disabled.ResponseSchema, body)
	if body["enabled"] != false {
		t.Fatalf("disabled response = %#v", body)
	}
}

func loadHTTPContract(t *testing.T) loadedHTTPContract {
	t.Helper()
	path := filepath.Join("..", "..", "tests", "fixtures", "http_api_v1.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture httpContractFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Version != 1 {
		t.Fatalf("HTTP contract version = %d", fixture.Version)
	}
	schemaData, err := os.ReadFile(filepath.Join("..", "..", "protocol", "http-v1.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(schemaData, &raw); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(raw["x-endpoints"])
	var endpoints map[string]httpContractEndpoint
	if err := json.Unmarshal(encoded, &endpoints); err != nil {
		t.Fatal(err)
	}
	for name, endpoint := range endpoints {
		fixtureCase, ok := fixture.Cases[name]
		if !ok {
			t.Fatalf("missing HTTP fixture case %s", name)
		}
		responseSchema, ok := endpoint.Responses[fmt.Sprint(fixtureCase.Status)]
		if !ok {
			t.Fatalf("missing response schema for %s status %d", name, fixtureCase.Status)
		}
		endpoint.Status = fixtureCase.Status
		endpoint.ResponseSchema = responseSchema
		endpoints[name] = endpoint
		if endpoint.Request == nil {
			if fixtureCase.Request != nil {
				t.Fatalf("unexpected request example for %s", name)
			}
		} else {
			assertJSONSchema(t, raw, *endpoint.Request, fixtureCase.Request)
		}
		assertJSONSchema(t, raw, responseSchema, fixtureCase.Response)
	}
	return loadedHTTPContract{Endpoints: endpoints, Schema: raw}
}

func contractRequest(t *testing.T, handler http.Handler, endpoint httpContractEndpoint, body string, cookie *http.Cookie, csrf, origin string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(endpoint.Method, endpoint.Path, reader)
	headers(request, "operator")
	if cookie != nil {
		request.AddCookie(cookie)
	}
	if csrf != "" {
		request.Header.Set("X-CSRF-Token", csrf)
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decodeExactJSON(t *testing.T, response *httptest.ResponseRecorder, keys []string, destination *map[string]any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), destination); err != nil {
		t.Fatalf("decode %q: %v", response.Body.String(), err)
	}
	if len(*destination) != len(keys) {
		t.Fatalf("response fields = %#v, want %v", *destination, keys)
	}
	for _, key := range keys {
		if _, ok := (*destination)[key]; !ok {
			t.Fatalf("missing response field %q in %#v", key, *destination)
		}
	}
}

func assertStatus(t *testing.T, response *httptest.ResponseRecorder, expected int) {
	t.Helper()
	if response.Code != expected {
		t.Fatalf("status=%d body=%q, want %d", response.Code, response.Body.String(), expected)
	}
}

func assertJSONBodySchema(t *testing.T, root map[string]any, reference, body string) {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(body), &value); err != nil {
		t.Fatal(err)
	}
	assertJSONSchema(t, root, reference, value)
}

func assertJSONSchema(t *testing.T, root map[string]any, reference string, value any) {
	t.Helper()
	schema := resolveJSONSchema(t, root, reference)
	if !validJSONSchemaValue(root, schema, value) {
		t.Fatalf("value %#v does not match %s", value, reference)
	}
}

func resolveJSONSchema(t *testing.T, root map[string]any, reference string) map[string]any {
	t.Helper()
	current := any(root)
	for _, part := range strings.Split(strings.TrimPrefix(reference, "#/"), "/") {
		object, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("invalid schema reference %s", reference)
		}
		current = object[part]
	}
	schema, ok := current.(map[string]any)
	if !ok {
		t.Fatalf("schema reference %s is not an object", reference)
	}
	return schema
}

func validJSONSchemaValue(root, schema map[string]any, value any) bool {
	if reference, ok := schema["$ref"].(string); ok {
		current := any(root)
		for _, part := range strings.Split(strings.TrimPrefix(reference, "#/"), "/") {
			current = current.(map[string]any)[part]
		}
		return validJSONSchemaValue(root, current.(map[string]any), value)
	}
	if constant, ok := schema["const"]; ok && fmt.Sprint(constant) != fmt.Sprint(value) {
		return false
	}
	if choices, ok := schema["enum"].([]any); ok {
		matched := false
		for _, choice := range choices {
			matched = matched || fmt.Sprint(choice) == fmt.Sprint(value)
		}
		if !matched {
			return false
		}
	}
	types := []string{}
	switch raw := schema["type"].(type) {
	case string:
		types = append(types, raw)
	case []any:
		for _, item := range raw {
			types = append(types, item.(string))
		}
	}
	if len(types) > 0 {
		matched := false
		for _, kind := range types {
			switch kind {
			case "object":
				_, matched = value.(map[string]any)
			case "string":
				_, matched = value.(string)
			case "boolean":
				_, matched = value.(bool)
			case "null":
				matched = value == nil
			case "array":
				_, matched = value.([]any)
			}
			if matched {
				break
			}
		}
		if !matched {
			return false
		}
	}
	if object, ok := value.(map[string]any); ok {
		properties, _ := schema["properties"].(map[string]any)
		if required, ok := schema["required"].([]any); ok {
			for _, field := range required {
				if _, exists := object[field.(string)]; !exists {
					return false
				}
			}
		}
		if schema["additionalProperties"] == false {
			for field := range object {
				if _, exists := properties[field]; !exists {
					return false
				}
			}
		}
		for field, item := range object {
			if property, exists := properties[field]; exists && !validJSONSchemaValue(root, property.(map[string]any), item) {
				return false
			}
		}
	}
	if array, ok := value.([]any); ok {
		if minimum, ok := schema["minItems"].(float64); ok && len(array) < int(minimum) {
			return false
		}
		if maximum, ok := schema["maxItems"].(float64); ok && len(array) > int(maximum) {
			return false
		}
		if schema["uniqueItems"] == true {
			seen := make(map[string]struct{}, len(array))
			for _, item := range array {
				encoded, _ := json.Marshal(item)
				if _, exists := seen[string(encoded)]; exists {
					return false
				}
				seen[string(encoded)] = struct{}{}
			}
		}
		if itemSchema, ok := schema["items"].(map[string]any); ok {
			for _, item := range array {
				if !validJSONSchemaValue(root, itemSchema, item) {
					return false
				}
			}
		}
	}
	if text, ok := value.(string); ok {
		if minimum, ok := schema["minLength"].(float64); ok && len([]rune(text)) < int(minimum) {
			return false
		}
		if maximum, ok := schema["maxLength"].(float64); ok && len([]rune(text)) > int(maximum) {
			return false
		}
		if pattern, ok := schema["pattern"].(string); ok && !regexp.MustCompile(pattern).MatchString(text) {
			return false
		}
		if schema["format"] == "uri" {
			parsed, err := url.Parse(text)
			if err != nil || parsed.Scheme == "" || parsed.Host == "" {
				return false
			}
		}
		if schema["format"] == "uuid" && !protocol.IsUUID(text) {
			return false
		}
		if schema["format"] == "date-time" {
			if _, err := time.Parse(time.RFC3339Nano, text); err != nil {
				return false
			}
		}
	}
	return true
}
