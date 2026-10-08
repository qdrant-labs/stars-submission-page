package internals

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"text/template"

	agent "github.com/OpenRouterTeam/go-agent"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/invopop/jsonschema"
)

const PromptTemplate string = `
You are tasked with evaluating a submission for the Qdrant Stars program,
which gathers people from the Qdrant community whose contributions (in terms
of content and events) we want to reward as we deem them high quality and
beneficial for Qdrant's image.
Not all content is the same, though, so your task is to identify
how good the quality of a content piece is and give suggestions
on how to improve it, if needed.
What we are looking for is:
- Guide-oriented content, with more prescriptive than descriptive guidance
- Genuinely new and original uses of Qdrant, showcasing new features
or highlighting features that really differentiate Qdrant from other players
in the field
- Opinionated and tasteful takes on vector search
- Evals and/or benchmarks on search quality and performance, especially
good if they really highlight how Qdrant improves them

Besides that, the most important signal is whether or not the text
sounds human or, at least, human-curated: we are not against
LLM-assisted content, but content that is entirely written
by an LLM is not beneficial for Qdrant, since there is
no incentive for a user to read them if an LLM can
produce that same output for them.

Considering all this, read the submission from this URL '{{.Url}}' using the 'web_fetch' tool.

The author described it as:
"""
{{.Description}}
"""

{{.Task }}

Respond with ONLY a single JSON object that matches the provided JSON schema.
Do not write anything before or after it: no preamble, no explanations, no
markdown, no code fences. Your whole answer must be that JSON object.
`
const SuggestionsModel string = "openai/gpt-5.6-terra"
const EvaluationsModel string = "openai/gpt-6-astra"

// text/template, not html/template: the prompt is plain text, and HTML escaping
// would mangle URLs (& -> &amp;) and quotes in the description.
var promptTemplate = template.Must(template.New("prompt").Parse(PromptTemplate))

type LlmInput struct {
	Url         string
	Description string
	Task        string
}

type LlmSuggestionsOutput struct {
	Suggestions     []string `json:"suggestions" jsonschema_description:"list of suggestions on how the content piece could be improved"`
	CouldBeImproved bool     `json:"could_be_improved" jsonschema_description:"whether or not the content piece could be improved"`
}

type LlmEvaluationOutput struct {
	Review string  `json:"review" jsonschema_description:"review of the content piece"`
	Rating float32 `json:"rating" jsonschema:"minimum=0,maximum=10" jsonschema_description:"rating of the content piece, between 0 (minimum) and 10 (maximum)"`
}

// MaxAgentSteps caps how many steps (model turns) the agent may take per review.
const MaxAgentSteps = 5

// maxLoggedOutput bounds how much of a malformed agent answer is written to the log.
const maxLoggedOutput = 500

type LLM struct {
	client            agent.ResponseSender
	webFetch          agent.Tool
	suggestionsSchema *components.FormatJSONSchemaConfig
	evaluationsSchema *components.FormatJSONSchemaConfig
}

// LLMOption customizes NewLLM.
type LLMOption func(*agent.OpenRouterOptions)

func NewLLM(opts ...LLMOption) (*LLM, error) {
	key, ok := os.LookupEnv("OPENROUTER_API_KEY")
	if !ok || key == "" {
		return nil, errors.New("could not find OPENROUTER_API_KEY in the environment")
	}
	o := agent.OpenRouterOptions{SDK: agent.SDKOptions{APIKey: key}}
	for _, opt := range opts {
		opt(&o)
	}
	return newLLM(agent.NewOpenRouter(o))
}

func newLLM(client agent.ResponseSender) (*LLM, error) {
	suggestionsSchema, err := getJsonSchema[LlmSuggestionsOutput]("suggestions", "A schema representing how improvement suggestions regarding content pieces should be structured")
	if err != nil {
		return nil, err
	}
	evaluationsSchema, err := getJsonSchema[LlmEvaluationOutput]("evaluation", "A schema representing how the evaluation of a content piece should be structured.")
	if err != nil {
		return nil, err
	}
	webFetch := agent.NewServerTool(agent.ServerToolConfig{
		Name:   "web_fetch",
		Config: components.CreateResponsesRequestToolUnionOpenrouterWebFetch(components.WebFetchServerTool{}),
	})
	return &LLM{client: client, webFetch: webFetch, suggestionsSchema: suggestionsSchema, evaluationsSchema: evaluationsSchema}, nil
}

func schemaToMap(v *jsonschema.Schema) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func getJsonSchema[T any](name, description string) (*components.FormatJSONSchemaConfig, error) {
	var val T
	// Inline the struct instead of emitting a root "$ref" into "$defs": providers
	// that implement structured outputs generally expect a self-contained object schema.
	r := &jsonschema.Reflector{DoNotReference: true, ExpandedStruct: true}
	schemaMap, err := schemaToMap(r.Reflect(val))
	if err != nil {
		return nil, err
	}
	delete(schemaMap, "$schema")
	delete(schemaMap, "$id")
	return &components.FormatJSONSchemaConfig{
		Name:        name,
		Description: &description,
		Schema:      schemaMap,
	}, nil
}

// complete runs the agent on the prompt (it may fetch the submission with the
// web_fetch tool, for at most MaxAgentSteps steps) and decodes its final JSON
// answer into out.
func (l *LLM) complete(ctx context.Context, model string, schema *components.FormatJSONSchemaConfig, input LlmInput, out any) error {
	var buf bytes.Buffer
	if err := promptTemplate.Execute(&buf, input); err != nil {
		return fmt.Errorf("could not build the prompt: %w", err)
	}
	format := components.CreateFormatsJSONSchema(*schema)
	toolChoice := components.CreateOpenAIResponsesToolChoiceUnionOpenAIResponsesToolChoiceRequired(components.OpenAIResponsesToolChoiceRequiredRequired)
	result, err := agent.CallModel(ctx, l.client, agent.CallModelInput{
		Model:    model,
		Input:    buf.String(),
		Tools:    []agent.Tool{l.webFetch},
		StopWhen: []agent.StopCondition{agent.StepCountIs(MaxAgentSteps)},
		MaxTurns: MaxAgentSteps,
		Request: components.ResponsesRequest{
			Text:       &components.TextExtendedConfig{Format: &format},
			ToolChoice: &toolChoice,
		},
	})
	if err != nil {
		return fmt.Errorf("an error occurred while starting the agent: %w", err)
	}
	text, err := result.Text(ctx)
	if err != nil {
		return fmt.Errorf("an error occurred while running the agent: %w", err)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("the agent produced an empty answer")
	}
	if err := json.Unmarshal([]byte(text), out); err != nil {
		log.Printf("agent output is not valid JSON (model %s), first %d chars: %q", model, maxLoggedOutput, truncate(text, maxLoggedOutput))
		return fmt.Errorf("could not parse the agent output as JSON: %w", err)
	}
	return nil
}

func (l *LLM) GetSuggestions(ctx context.Context, url, description string) (*LlmSuggestionsOutput, error) {
	var out LlmSuggestionsOutput
	err := l.complete(ctx, SuggestionsModel, l.suggestionsSchema, LlmInput{
		Task:        "Evaluate whether the content piece could be improved and, if so, provide a list of suggestions on how to do so. If the content is already good to go, leave the list of suggestions empty. Don't be excessively strict, and stick to the guidance above.",
		Url:         url,
		Description: description,
	}, &out)
	if err != nil {
		return nil, err
	}
	if out.Suggestions == nil {
		out.Suggestions = []string{}
	}
	return &out, nil
}

func (l *LLM) GetEvaluation(ctx context.Context, url, description string) (*LlmEvaluationOutput, error) {
	var out LlmEvaluationOutput
	err := l.complete(ctx, EvaluationsModel, l.evaluationsSchema, LlmInput{
		Task:        "Provide a general review of the content piece, highlighting strengths and weaknesses, and rate it between 0 and 10, where 0 means 'gets no reward' and 10 means 'gets maximum reward'. Don't be excessively strict, and stick to the guidance above. The review is required in every case: always write it, whether the content is excellent, mediocre or poor, and never leave it empty.",
		Url:         url,
		Description: description,
	}, &out)
	if err != nil {
		return nil, err
	}
	// The schema bounds are only a hint to the model; enforce them.
	out.Rating = min(max(out.Rating, 0), 10)
	return &out, nil
}
