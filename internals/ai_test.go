package internals

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
)

// fakeSender stands in for the Responses API: it records the requests it gets
// and answers each with a fixed final text (or error).
type fakeSender struct {
	text string
	err  error
	reqs []components.ResponsesRequest
}

func (f *fakeSender) SendResponse(_ context.Context, req components.ResponsesRequest, _ *components.MetadataLevel, _ ...operations.Option) (*operations.CreateResponsesResponse, error) {
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return nil, f.err
	}
	res := operations.CreateCreateResponsesResponseOpenResponsesResult(components.OpenResponsesResult{OutputText: &f.text})
	return &res, nil
}

func newTestLLM(t *testing.T, f *fakeSender) *LLM {
	t.Helper()
	l, err := newLLM(f)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestEvaluationClampedAndRequestShape(t *testing.T) {
	f := &fakeSender{text: `{"review":"solid","rating":42}`}
	l := newTestLLM(t, f)
	url := "https://example.com/post?a=1&b=2"
	got, err := l.GetEvaluation(context.Background(), url, `it's "great" & new`)
	if err != nil {
		t.Fatal(err)
	}
	if got.Rating != 10 || got.Review != "solid" {
		t.Fatalf("%+v", got)
	}
	if len(f.reqs) != 1 {
		t.Fatalf("requests = %d", len(f.reqs))
	}
	if f.reqs[0].Model == nil || *f.reqs[0].Model != EvaluationsModel {
		t.Errorf("model = %v", f.reqs[0].Model)
	}
	raw, _ := json.Marshal(f.reqs[0])
	var body struct {
		Input []struct {
			Content string `json:"content"`
		} `json:"input"`
		Tools      []map[string]any `json:"tools"`
		ToolChoice string           `json:"tool_choice"`
		Text       struct {
			Format struct {
				Type string `json:"type"`
			} `json:"format"`
		} `json:"text"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tools) != 1 || body.Tools[0]["type"] != "openrouter:web_fetch" {
		t.Errorf("tools = %v, want only the web_fetch server tool", body.Tools)
	}
	if body.ToolChoice != "required" || body.Text.Format.Type != "json_schema" {
		t.Errorf("tool_choice = %q, format = %q", body.ToolChoice, body.Text.Format.Type)
	}
	// the prompt must carry the URL and description verbatim (no HTML escaping)
	if len(body.Input) != 1 || !strings.Contains(body.Input[0].Content, url) || !strings.Contains(body.Input[0].Content, `it's "great" & new`) {
		t.Errorf("prompt is missing the URL/description verbatim: %q", body.Input)
	}
}

func TestSuggestionsNilSliceBecomesEmpty(t *testing.T) {
	l := newTestLLM(t, &fakeSender{text: `{"could_be_improved":false}`})
	got, err := l.GetSuggestions(context.Background(), "https://example.com", "d")
	if err != nil {
		t.Fatal(err)
	}
	if got.Suggestions == nil || len(got.Suggestions) != 0 || got.CouldBeImproved {
		t.Fatalf("%+v", got)
	}
}

func TestBadAgentOutputIsAnError(t *testing.T) {
	for name, f := range map[string]*fakeSender{
		"empty":     {text: ""},
		"not json":  {text: "not json"},
		"api error": {err: errors.New("boom")},
	} {
		l := newTestLLM(t, f)
		if _, err := l.GetEvaluation(context.Background(), "https://example.com", "d"); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestSchemaIsSelfContained(t *testing.T) {
	l := newTestLLM(t, &fakeSender{})
	b, _ := json.Marshal(l.evaluationsSchema.Schema)
	s := string(b)
	for _, bad := range []string{`"$ref"`, `"$defs"`, `"$schema"`} {
		if strings.Contains(s, bad) {
			t.Errorf("schema contains %s: %s", bad, s)
		}
	}
	for _, want := range []string{`"maximum":10`, `"minimum":0`, `review of the content piece`, `"additionalProperties":false`} {
		if !strings.Contains(s, want) {
			t.Errorf("schema is missing %s: %s", want, s)
		}
	}
}

func TestAgentStepCap(t *testing.T) {
	if MaxAgentSteps != 5 {
		t.Fatalf("MaxAgentSteps = %d, want 5", MaxAgentSteps)
	}
}
