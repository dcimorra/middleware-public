package agents

import (
	"net/http"
	"path/filepath"

	"github.com/dcimorra/middleware-public/internal/agent"
	"github.com/dcimorra/middleware-public/internal/agent/opencode"
	"github.com/dcimorra/middleware-public/internal/modules"
)

// Vendor is a model maker with a module of its own: its key, its models,
// a card in Modules and an agent in the chat, as Claude and Gemini have.
// None of them ships an agent of its own that runs on a server the way
// Claude Code does, so the agent is OpenCode, fixed to that one vendor:
// the same tools, the same jail, the same turns, a different model.
type Vendor struct {
	// Key is the agent's name in the chat and the manifests ("deepseek").
	Key string
	// Provider is OpenCode's id for the vendor ("deepseek", "moonshotai").
	Provider string
	Name     string
	Desc     string
	// KeyHelp says where the key comes from.
	KeyHelp string
}

// Vendors are the best known, and only those: every other provider is in
// the OpenCode module's catalog.
var Vendors = []Vendor{
	{Key: "openai", Provider: "openai", Name: "OpenAI", Desc: "GPT models (GPT-4.1, o-series…) with an OpenAI API key.", KeyHelp: "From platform.openai.com → API keys."},
	{Key: "deepseek", Provider: "deepseek", Name: "DeepSeek", Desc: "DeepSeek's models with a DeepSeek API key.", KeyHelp: "From platform.deepseek.com → API keys."},
	{Key: "zai", Provider: "zai", Name: "Z.AI (GLM)", Desc: "Zhipu's GLM models with a Z.AI API key.", KeyHelp: "From z.ai → API keys. A GLM Coding Plan key works too."},
	{Key: "kimi", Provider: "moonshotai", Name: "Kimi (Moonshot)", Desc: "Moonshot's Kimi models with a Moonshot API key.", KeyHelp: "From platform.moonshot.ai → API keys."},
	{Key: "qwen", Provider: "alibaba", Name: "Qwen (Alibaba)", Desc: "Alibaba's Qwen models with a DashScope (Model Studio) API key.", KeyHelp: "From Alibaba Cloud Model Studio → API keys (DASHSCOPE_API_KEY)."},
	{Key: "mistral", Provider: "mistral", Name: "Mistral", Desc: "Mistral's models (Mistral Large, Codestral, Devstral…) with a Mistral API key.", KeyHelp: "From console.mistral.ai → API keys."},
	{Key: "xai", Provider: "xai", Name: "xAI (Grok)", Desc: "xAI's Grok models with an xAI API key.", KeyHelp: "From console.x.ai → API keys."},
}

// NewVendor builds a vendor's module over reg, with a HOME of its own
// under homes (data/agents/<key>).
func NewVendor(reg *agent.Registry, homes string, v Vendor) *OpenCode {
	o := &OpenCode{Home: filepath.Join(homes, v.Key), vendor: v.Provider}
	m := &Module{
		key:  "agent-" + v.Key,
		name: v.Name,
		desc: v.Desc,
		bin:  "opencode",
		needs: []modules.Need{
			{Key: "api_key", Label: "API key", Secret: true, Help: v.KeyHelp},
			{Key: "model", Label: "Default model", Optional: true, Help: "Offered first in the chat, as " + v.Provider + "/<model>. Empty: the first one listed."},
			{Key: "bin", Label: "Binary", Optional: true, Help: "Path to the opencode executable that runs the turns. Empty: the one this installation keeps under modules/node, installed and updated for you."},
		},
		reg:       reg,
		secretKey: "api_key",
		// Off, the vendor leaves the chat: nothing could run it.
		removeOnDisable: true,
	}
	m.build = func(cfg modules.Config) agent.Provider {
		auth := o.authOf(cfg)
		return opencode.New(opencode.Config{Key: v.Key, Bin: cfg["bin"], Auth: auth, Models: modelsWith(cfg["model"], o.connectedModels(auth))})
	}
	o.Module = m
	m.init()
	o.loadCachedModels()
	return o
}

// vendorRoutes are a vendor module's: its models and its binary.
func (o *OpenCode) vendorRoutes() map[string]http.Handler {
	return map[string]http.Handler{
		"GET /{$}":             http.HandlerFunc(o.handleProviders),
		"GET /models":          http.HandlerFunc(o.handleProviders),
		"POST /models/refresh": http.HandlerFunc(o.handleRefresh),
		"POST /binary/sync":    http.HandlerFunc(o.handleBinarySync),
	}
}
