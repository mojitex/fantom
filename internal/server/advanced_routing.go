package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mojitex/fantom/internal/config"
	"github.com/mojitex/fantom/internal/routing"
	"github.com/mojitex/fantom/internal/optimizer"
)

type requiredCapabilitySet map[string]bool

type smartSessionEntry struct {
	Model     string
	UpdatedAt time.Time
}

type smartDecision struct {
	Choice        string             `json:"choice"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

type fusionUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
}

type fusionPanelResult struct {
	Model        string
	Provider     string
	Upstream     string
	Text         string
	Usage        fusionUsage
	Attempts     int
	Status       int
	CostUSD      float64
	CostKnown    bool
	Err          error
}

func (s *Server) providerMap(state adminState) map[string]config.Provider {
	out := make(map[string]config.Provider, len(state.Providers))
	for _, item := range state.Providers {
		provider := item.runtime()
		out[provider.ID] = provider
	}
	return out
}

func requiredCapabilities(payload map[string]json.RawMessage) requiredCapabilitySet {
	required := requiredCapabilitySet{}
	addMime := func(mime string) {
		mime = strings.ToLower(strings.TrimSpace(mime))
		switch {
		case strings.HasPrefix(mime, "image/"):
			required["vision"] = true
		case mime == "application/pdf":
			required["pdf"] = true
		case strings.HasPrefix(mime, "audio/"):
			required["audio_input"] = true
		case strings.HasPrefix(mime, "video/"):
			required["video_input"] = true
		}
	}
	var scanAny func(any)
	scanAny = func(value any) {
		switch item := value.(type) {
		case []any:
			for _, child := range item { scanAny(child) }
		case map[string]any:
			kind, _ := item["type"].(string)
			switch strings.ToLower(kind) {
			case "image", "image_url", "input_image":
				required["vision"] = true
			case "input_audio", "audio", "audio_url":
				required["audio_input"] = true
			case "input_video", "video", "video_url":
				required["video_input"] = true
			case "file", "document", "input_file":
				mime := ""
				if source, ok := item["source"].(map[string]any); ok {
					mime, _ = source["media_type"].(string)
				}
				if file, ok := item["file"].(map[string]any); ok {
					if data, ok := file["file_data"].(string); ok && strings.HasPrefix(data, "data:") {
						mime = strings.Split(strings.TrimPrefix(data, "data:"), ";")[0]
					}
				}
				if mime == "" { required["pdf"] = true } else { addMime(mime) }
			}
			for _, key := range []string{"mime_type", "mimeType", "media_type"} {
				if mime, ok := item[key].(string); ok { addMime(mime) }
			}
			for _, child := range item { scanAny(child) }
		}
	}

	if raw := payload["tools"]; len(raw) > 0 && string(raw) != "null" && string(raw) != "[]" {
		required["tools"] = true
		if bytes.Contains(bytes.ToLower(raw), []byte("search")) || bytes.Contains(bytes.ToLower(raw), []byte("browser")) {
			required["search"] = true
		}
	}
	for _, key := range []string{"reasoning_effort", "thinking", "reasoning"} {
		if raw := payload[key]; len(raw) > 0 && string(raw) != "null" && string(raw) != ""off"" {
			required["reasoning"] = true
		}
	}

	var messages []map[string]any
	if raw := payload["messages"]; len(raw) > 0 {
		_ = json.Unmarshal(raw, &messages)
		if len(messages) > 0 {
			lastAssistant := -1
			for i, message := range messages {
				role, _ := message["role"].(string)
				if role == "assistant" || role == "model" { lastAssistant = i }
			}
			for _, message := range messages[lastAssistant+1:] {
				role, _ := message["role"].(string)
				if role == "" || role == "user" { scanAny(message) }
			}
		}
	}
	var input []any
	if raw := payload["input"]; len(raw) > 0 { _ = json.Unmarshal(raw, &input) }
	if len(input) > 0 {
		lastAssistant := -1
		for i, item := range input {
			if message, ok := item.(map[string]any); ok {
				role, _ := message["role"].(string)
				if role == "assistant" { lastAssistant = i }
			}
		}
		for _, item := range input[lastAssistant+1:] { scanAny(item) }
	}
	var contents []any
	if raw := payload["contents"]; len(raw) > 0 { _ = json.Unmarshal(raw, &contents) }
	if len(contents) > 0 {
		lastModel := -1
		for i, item := range contents {
			if message, ok := item.(map[string]any); ok {
				role, _ := message["role"].(string)
				if role == "model" { lastModel = i }
			}
		}
		for _, item := range contents[lastModel+1:] { scanAny(item) }
	}
	if raw := payload["images"]; len(raw) > 0 && string(raw) != "null" && string(raw) != "[]" {
		required["vision"] = true
	}
	if raw := payload["image_url"]; len(raw) > 0 { required["vision"] = true }
	if raw := payload["audio"]; len(raw) > 0 { required["audio_input"] = true }
	return required
}

func modelCapabilities(provider config.Provider, requestedModel, upstreamModel string) config.ModelCapabilities {
	if caps, ok := provider.ModelCapabilities[upstreamModel]; ok { return caps }
	if caps, ok := provider.ModelCapabilities[requestedModel]; ok { return caps }

	model := strings.ToLower(upstreamModel)
	caps := config.ModelCapabilities{Tools: true}
	if containsAny(model, "vision", "-vl", "/vl", "llava", "pixtral", "qwen-vl", "gemini", "claude", "gpt-4o", "gpt-5", "gpt-6") {
		caps.Vision = true
	}
	if containsAny(model, "gemini", "claude", "gpt-4o", "gpt-5", "gpt-6", "pdf") {
		caps.PDF = true
	}
	if containsAny(model, "audio", "omni", "voice", "whisper", "transcri", "speech") {
		caps.AudioInput = true
	}
	if containsAny(model, "video", "gemini") {
		caps.VideoInput = true
	}
	if containsAny(model, "reason", "thinking", "deepseek-r1", "deepseek-v", "qwq", "o1", "o3", "o4", "glm-5") {
		caps.Reasoning = true
	}
	if containsAny(model, "search", "sonar", "perplexity") {
		caps.Search = true
	}
	return caps
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) { return true }
	}
	return false
}

func (s *Server) candidateCapabilities(candidate string, state adminState, providers map[string]config.Provider) config.ModelCapabilities {
	target, err := routing.Resolve(candidate, state.Routes, state.DefaultProvider, providers)
	if err == nil {
		provider := providers[target.ProviderID]
		return modelCapabilities(provider, candidate, target.UpstreamModel)
	}
	if slash := strings.Index(candidate, "/"); slash > 0 {
		providerID := candidate[:slash]
		if provider, ok := providers[providerID]; ok {
			return modelCapabilities(provider, candidate, candidate[slash+1:])
		}
	}
	return config.ModelCapabilities{}
}

func (s *Server) reorderByCapabilities(candidates []string, required requiredCapabilitySet, state adminState, providers map[string]config.Provider) []string {
	if len(candidates) < 2 || len(required) == 0 { return candidates }
	type ranked struct { model string; tier int; soft int; index int }
	hard := []string{"vision", "pdf", "audio_input", "video_input"}
	soft := []string{"search", "tools", "reasoning"}
	rankedModels := make([]ranked, 0, len(candidates))
	for index, model := range candidates {
		caps := s.candidateCapabilities(model, state, providers)
		missingHard := false
		for _, key := range hard {
			if required[key] && !capabilityValue(caps, key) { missingHard = true; break }
		}
		softScore := 0
		for _, key := range soft {
			if required[key] && capabilityValue(caps, key) { softScore++ }
		}
		tier := 0
		if missingHard { tier = 2 } else if softScore < countRequired(required, soft) { tier = 1 }
		rankedModels = append(rankedModels, ranked{model:model,tier:tier,soft:softScore,index:index})
	}
	sort.SliceStable(rankedModels, func(i,j int) bool {
		if rankedModels[i].tier != rankedModels[j].tier { return rankedModels[i].tier < rankedModels[j].tier }
		if rankedModels[i].soft != rankedModels[j].soft { return rankedModels[i].soft > rankedModels[j].soft }
		return rankedModels[i].index < rankedModels[j].index
	})
	out := make([]string, len(rankedModels))
	for i, item := range rankedModels { out[i] = item.model }
	return out
}

func capabilityValue(caps config.ModelCapabilities, name string) bool {
	switch name {
	case "vision": return caps.Vision
	case "pdf": return caps.PDF
	case "audio_input": return caps.AudioInput
	case "video_input": return caps.VideoInput
	case "search": return caps.Search
	case "tools": return caps.Tools
	case "reasoning": return caps.Reasoning
	default: return false
	}
}

func countRequired(required requiredCapabilitySet, keys []string) int {
	count := 0
	for _, key := range keys { if required[key] { count++ } }
	return count
}

func (s *Server) applyCapacityAdapters(candidates []string, required requiredCapabilitySet, state adminState, providers map[string]config.Provider) []string {
	hard := []string{"vision", "pdf", "audio_input", "video_input"}
	hasHard := false
	for _, key := range hard { if required[key] { hasHard = true } }
	if !hasHard || len(candidates) == 0 { return candidates }
	for _, candidate := range candidates {
		caps := s.candidateCapabilities(candidate, state, providers)
		satisfied := true
		for _, key := range hard {
			if required[key] && !capabilityValue(caps, key) { satisfied = false; break }
		}
		if satisfied { return candidates }
	}
	pool := make([]string, 0)
	seen := map[string]bool{}
	for _, key := range hard {
		if !required[key] { continue }
		for _, model := range state.CapacityAdapters[key] {
			if !seen[model] {
				caps := s.candidateCapabilities(model, state, providers)
				satisfied := true
				for _, hardKey := range hard {
					if required[hardKey] && !capabilityValue(caps, hardKey) { satisfied = false; break }
				}
				if satisfied { pool = append(pool, model); seen[model] = true }
			}
		}
	}
	if len(pool) == 0 { return candidates }
	for _, candidate := range candidates { if !seen[candidate] { pool = append(pool, candidate); seen[candidate] = true } }
	return pool
}

func extractRequestSession(r *http.Request, payload map[string]json.RawMessage) string {
	for _, header := range []string{"X-OpenCode-Session", "X-Session-ID", "X-Conversation-ID", "X-Request-Session"} {
		value := strings.TrimSpace(r.Header.Get(header))
		if value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\r\n") { return value }
	}
	var root map[string]any
	raw, _ := json.Marshal(payload)
	if json.Unmarshal(raw, &root) == nil {
		for _, path := range [][]string{{"session_id"}, {"metadata","session_id"}, {"conversation_id"}} {
			var current any = root
			for _, key := range path {
				m, ok := current.(map[string]any); if !ok { current = nil; break }
				current = m[key]
			}
			if value, ok := current.(string); ok && strings.TrimSpace(value) != "" && len(value) <= 256 {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

func extractPromptText(payload map[string]json.RawMessage, maxChars int) string {
	var parts []string
	appendText := func(value any) {
		switch item := value.(type) {
		case string:
			if strings.TrimSpace(item) != "" { parts = append(parts, strings.TrimSpace(item)) }
		case []any:
			for _, child := range item {
				if block, ok := child.(map[string]any); ok {
					if text, ok := block["text"].(string); ok && strings.TrimSpace(text) != "" { parts = append(parts, strings.TrimSpace(text)) }
				}
			}
		}
	}
	var messages []map[string]any
	if raw := payload["messages"]; len(raw) > 0 {
		_ = json.Unmarshal(raw, &messages)
		for i:=len(messages)-1; i>=0; i-- {
			role,_:=messages[i]["role"].(string)
			if role=="user" { appendText(messages[i]["content"]); break }
		}
	}
	if len(parts)==0 {
		var prompt string
		if raw:=payload["prompt"];len(raw)>0 { _=json.Unmarshal(raw,&prompt) }
		if raw:=payload["input"];len(raw)>0 && prompt=="" { _=json.Unmarshal(raw,&prompt) }
		if strings.TrimSpace(prompt)!="" { parts=append(parts,strings.TrimSpace(prompt)) }
	}
	text:=strings.Join(parts,"\n")
	if len(text)>maxChars { text=text[:maxChars] }
	return text
}

func (s *Server) smartOrderCandidates(ctx context.Context, r *http.Request, payload map[string]json.RawMessage, comboName string, route config.Route, candidates []string, state adminState, providers map[string]config.Provider) ([]string, bool) {
	if route.Strategy != "smart" || len(candidates)<2 { return candidates,false }
	session:=extractRequestSession(r,payload)
	key:=""
	if session!="" { key=comboName+":"+session }
	var previous string
	if key!="" {
		if raw,ok:=s.smartSessions.Load(key);ok {
			entry:=raw.(smartSessionEntry)
			if time.Since(entry.UpdatedAt) <= 2*time.Hour && containsString(candidates,entry.Model) { previous=entry.Model } else { s.smartSessions.Delete(key) }
		}
	}
	if route.SmartPolicy=="strict-lock" && previous!="" {
		return moveCandidateFirst(candidates,previous),true
	}

	decisionProvider:=s.providerByID(route.SmartDecisionProvider)
	if decisionProvider==nil { return applyStickyDecision(candidates,previous),false }
	decision:=s.requestSmartDecision(ctx,decisionProvider,route.SmartDecisionModel,payload,candidates,route,providers)
	if decision==nil { return applyStickyDecision(candidates,previous),false }
	choice:=canonicalCandidate(candidates,decision.Choice,state,providers)
	if choice=="" { return applyStickyDecision(candidates,previous),false }

	selected:=choice
	if route.SmartPolicy=="smart-switch" && previous!="" && decision.Confidence < route.SmartConfidence { selected=previous }
	if route.SmartPolicy=="smart-switch" && decision.Confidence < route.SmartConfidence && previous=="" {
		return candidates,false
	}
	ordered:=orderCandidatesByDecision(candidates,selected,decision.Probabilities,state,providers)
	if key!="" && route.SmartPolicy!="per-turn" {
		s.smartSessions.Store(key,smartSessionEntry{Model:selected,UpdatedAt:time.Now()})
		if lenCandidates:=len(candidates); lenCandidates>0 && int(s.smartSessionSweeps.Add(1))%128==0 {
			count:=0
			s.smartSessions.Range(func(k,v any)bool{
				count++
				if entry,ok:=v.(smartSessionEntry);ok && time.Since(entry.UpdatedAt)>2*time.Hour { s.smartSessions.Delete(k) }
				if count>6000 { return false }
				return true
			})
		}
	}
	return ordered,true
}

func applyStickyDecision(candidates []string, previous string) []string {
	if previous!="" { return moveCandidateFirst(candidates,previous) }
	return candidates
}

func (s *Server) requestSmartDecision(parent context.Context, decisionProvider *providerRuntime, decisionModel string, payload map[string]json.RawMessage, candidates []string, route config.Route, providers map[string]config.Provider) *smartDecision {
	timeout:=time.Duration(route.SmartTimeoutSeconds)*time.Second
	if timeout<=0 { timeout=8*time.Second }
	ctx,cancel:=context.WithTimeout(parent,timeout)
	defer cancel()
	criteria:=make([]map[string]any,0,len(candidates))
	for _,candidate:=range candidates {
		target,err:=routing.Resolve(candidate,s.store.snapshot().Routes,s.store.snapshot().DefaultProvider,providers)
		if err!=nil { continue }
		caps:=s.candidateCapabilities(candidate,s.store.snapshot(),providers)
		criteria=append(criteria,map[string]any{"id":candidate,"provider":target.ProviderID,"upstream_model":target.UpstreamModel,"capabilities":caps})
	}
	criteriaJSON,_:=json.Marshal(criteria)
	prompt:=extractPromptText(payload,2000)
	system:="You are a deterministic AI model router. Choose the most suitable candidate for the user's request. Return JSON only: {\"choice\":\"exact candidate id\",\"confidence\":0.0-1.0,\"probabilities\":{\"candidate id\":0.0-1.0}}. Never invent a candidate. Consider task complexity, coding ability, modalities and reasoning requirements."
	user:=fmt.Sprintf("Candidates: %s\nRequest: %s",criteriaJSON,prompt)
	body,_:=json.Marshal(map[string]any{"model":decisionModel,"temperature":0,"max_tokens":256,"stream":false,"messages":[]map[string]string{{"role":"system","content":system},{"role":"user","content":user}}})
	var rawResponse []byte
	for _, key:=range decisionProvider.apiKeysForRequest() {
		if decisionProvider.config.AuthType=="bearer" && key=="" { break }
		req,err:=http.NewRequestWithContext(ctx,http.MethodPost,decisionProvider.config.BaseURL+decisionProvider.config.ChatPath,bytes.NewReader(body))
		if err!=nil { return nil }
		req.Header.Set("Content-Type","application/json")
		req.Header.Set("Accept","application/json")
		s.applyProviderHeaders(req,decisionProvider,key)
		resp,err:=decisionProvider.client.Do(req)
		if err!=nil { if ctx.Err()!=nil { return nil }; continue }
		status:=resp.StatusCode
		if shouldRetryCredential(status) {
			decisionProvider.cooldownCredential(key,status,resp.Header.Get("Retry-After"))
			resp.Body.Close()
			continue
		}
		rawResponse,_=io.ReadAll(io.LimitReader(resp.Body,1<<20))
		resp.Body.Close()
		if status<200||status>=300 { return nil }
		break
	}
	if len(rawResponse)==0 { return nil }
	var envelope struct { Choices []struct{ Message struct{Content string `json:"content"`} `json:"message"` } `json:"choices"` }
	if json.Unmarshal(rawResponse,&envelope)!=nil || len(envelope.Choices)==0 { return nil }
	content:=extractJSONObject(envelope.Choices[0].Message.Content)
	if content=="" { return nil }
	var parsed struct {
		Choice string `json:"choice"`
		Confidence float64 `json:"confidence"`
		Probabilities map[string]float64 `json:"probabilities"`
		Answers struct { Route struct {
			Choice string `json:"choice"`
			Confidence float64 `json:"confidence"`
			Probabilities map[string]float64 `json:"probabilities"`
		} `json:"route"` } `json:"answers"`
	}
	if json.Unmarshal([]byte(content),&parsed)!=nil { return nil }
	if parsed.Choice=="" {
		parsed.Choice=parsed.Answers.Route.Choice
		parsed.Confidence=parsed.Answers.Route.Confidence
		parsed.Probabilities=parsed.Answers.Route.Probabilities
	}
	if parsed.Choice=="" || parsed.Confidence<0 || parsed.Confidence>1 { return nil }
	return &smartDecision{Choice:parsed.Choice,Confidence:parsed.Confidence,Probabilities:parsed.Probabilities}
}

func extractJSONObject(input string) string {
	input=strings.TrimSpace(input)
	if strings.HasPrefix(input,"```") {
		input=strings.TrimPrefix(input,"```json")
		input=strings.TrimPrefix(input,"```")
		input=strings.TrimSuffix(input,"```")
		input=strings.TrimSpace(input)
	}
	start:=strings.Index(input,"{")
	end:=strings.LastIndex(input,"}")
	if start<0||end<=start { return "" }
	return input[start:end+1]
}

func canonicalCandidate(candidates []string, proposed string, state adminState, providers map[string]config.Provider) string {
	proposed=strings.TrimSpace(proposed)
	if containsString(candidates,proposed) { return proposed }
	for _,candidate:=range candidates {
		target,err:=routing.Resolve(candidate,state.Routes,state.DefaultProvider,providers)
		if err==nil && target.UpstreamModel==proposed { return candidate }
	}
	return ""
}

func orderCandidatesByDecision(candidates []string, selected string, probabilities map[string]float64, state adminState, providers map[string]config.Provider) []string {
	out:=append([]string(nil),candidates...)
	if selected=="" { return out }
	selectedIndex:=-1
	for i,candidate:=range out { if candidate==selected { selectedIndex=i;break } }
	if selectedIndex<0 { return out }
	chosen:=out[selectedIndex]
	remaining:=append(out[:selectedIndex],out[selectedIndex+1:]...)
	if len(probabilities)>0 {
		sort.SliceStable(remaining,func(i,j int)bool{
			pi:=probabilities[remaining[i]]
			pj:=probabilities[remaining[j]]
			if pi==pj { return false }
			return pi>pj
		})
	}
	return append([]string{chosen},remaining...)
}

func containsString(items []string, value string) bool {
	for _, item:=range items { if item==value { return true } }
	return false
}

func moveCandidateFirst(items []string, value string) []string {
	for index, item := range items {
		if item != value { continue }
		out := make([]string, 0, len(items))
		out = append(out, value)
		out = append(out, items[:index]...)
		out = append(out, items[index+1:]...)
		return out
	}
	return items
}

func (s *Server) estimatedModelCost(provider config.Provider, upstreamModel string, promptTokens, completionTokens int64) float64 {
	pricing,ok:=provider.ModelPricing[upstreamModel]
	if !ok { return 0 }
	return float64(promptTokens)*pricing.InputPerMillionUSD/1_000_000 + float64(completionTokens)*pricing.OutputPerMillionUSD/1_000_000
}

func resolveCandidate(model string, state adminState, providers map[string]config.Provider) (routing.Target,error) {
	target,err:=routing.Resolve(model,state.Routes,state.DefaultProvider,providers)
	if err==nil { return target,nil }
	if slash:=strings.Index(model,"/");slash>0 {
		providerID:=model[:slash]
		if provider,ok:=providers[providerID];ok && modelAllowed(provider.Models,model[slash+1:]) {
			return routing.Target{ProviderID:providerID,UpstreamModel:model[slash+1:]},nil
		}
	}
	return routing.Target{},err
}

func clonePayloadWithoutTools(payload map[string]json.RawMessage) map[string]json.RawMessage {
	out:=make(map[string]json.RawMessage,len(payload))
	for key,value:=range payload {
		switch key { case "tools","tool_choice","stream_options": continue }
		out[key]=value
	}
	out["stream"]=json.RawMessage("false")
	return out
}

func flattenFusionMessages(payload map[string]json.RawMessage) {
	raw:=payload["messages"]
	if len(raw)==0 { return }
	var messages []map[string]any
	if json.Unmarshal(raw,&messages)!=nil { return }
	for _,message:=range messages {
		role,_:=message["role"].(string)
		if role=="tool"||role=="function" {
			content,_:=message["content"].(string)
			message["role"]="assistant"
			message["content"]="[Tool result: "+content+"]"
			continue
		}
		if calls,ok:=message["tool_calls"].([]any);ok && len(calls)>0 {
			names:=make([]string,0,len(calls))
			for _,call:=range calls {
				if obj,ok:=call.(map[string]any);ok {
					if fn,ok:=obj["function"].(map[string]any);ok { if name,ok:=fn["name"].(string);ok { names=append(names,name) } }
				}
			}
			base,_:=message["content"].(string)
			if len(names)>0 { message["content"]=strings.TrimSpace(base+"\n[Called tools: "+strings.Join(names,", ")+"]") }
			delete(message,"tool_calls")
		}
	}
	if encoded,err:=json.Marshal(messages);err==nil { payload["messages"]=encoded }
}

func extractChatText(body []byte) string {
	var envelope map[string]any
	if json.Unmarshal(body,&envelope)!=nil { return "" }
	if choices,ok:=envelope["choices"].([]any);ok && len(choices)>0 {
		if choice,ok:=choices[0].(map[string]any);ok {
			message,_:=choice["message"].(map[string]any)
			if content,ok:=message["content"].(string);ok { return strings.TrimSpace(content) }
			if content,ok:=message["content"].([]any);ok {
				var parts []string
				for _,part:=range content {
					if item,ok:=part.(map[string]any);ok { if text,ok:=item["text"].(string);ok { parts=append(parts,text) } }
				}
				return strings.TrimSpace(strings.Join(parts,"\n"))
			}
		}
	}
	if text,ok:=envelope["output_text"].(string);ok { return strings.TrimSpace(text) }
	return ""
}

func extractFusionUsage(body []byte) fusionUsage {
	var envelope struct { Usage fusionUsage `json:"usage"` }
	_ = json.Unmarshal(body,&envelope)
	u:=envelope.Usage
	if u.PromptTokens==0 { u.PromptTokens=u.InputTokens }
	if u.CompletionTokens==0 { u.CompletionTokens=u.OutputTokens }
	if u.TotalTokens==0 { u.TotalTokens=u.PromptTokens+u.CompletionTokens }
	return u
}

func (s *Server) requestFusionModel(ctx context.Context, incoming *http.Request, payload map[string]json.RawMessage, model string, state adminState, providers map[string]config.Provider, saverMode, codeMode string) fusionPanelResult {
	result:=fusionPanelResult{Model:model}
	target,err:=resolveCandidate(model,state,providers)
	if err!=nil { result.Err=err; return result }
	result.Provider=target.ProviderID
	result.Upstream=target.UpstreamModel
	provider:=s.providerByID(target.ProviderID)
	if provider==nil { result.Err=fmt.Errorf("provider %q is unavailable",target.ProviderID); return result }
	attemptPayload:=make(map[string]json.RawMessage,len(payload)+1)
	for key,value:=range payload { attemptPayload[key]=value }
	modelJSON,_:=json.Marshal(target.UpstreamModel)
	attemptPayload["model"]=modelJSON
	attemptBody,_:=json.Marshal(attemptPayload)
	attemptBody,_=optimizer.Apply(attemptBody,saverMode,codeMode)

	for _,key:=range provider.apiKeysForRequest() {
		if provider.config.AuthType=="bearer"&&key=="" { break }
		req,err:=http.NewRequestWithContext(ctx,http.MethodPost,provider.config.BaseURL+provider.config.ChatPath,bytes.NewReader(attemptBody))
		if err!=nil { result.Err=err;return result }
		req.Header.Set("Content-Type","application/json")
		req.Header.Set("Accept","application/json")
		req.Header.Set("X-Request-ID",incoming.Header.Get("X-Request-ID"))
		if provider.config.ID=="opencode-console" {
			if session:=strings.TrimSpace(incoming.Header.Get("X-OpenCode-Session"));session!="" {req.Header.Set("x-opencode-session",session)}
		}
		s.applyProviderHeaders(req,provider,key)
		result.Attempts++
		resp,err:=provider.client.Do(req)
		if err!=nil { result.Err=err; if ctx.Err()!=nil{return result}; continue }
		result.Status=resp.StatusCode
		if shouldRetryCredential(resp.StatusCode) {
			provider.cooldownCredential(key,resp.StatusCode,resp.Header.Get("Retry-After"))
			resp.Body.Close()
			continue
		}
		body,readErr:=io.ReadAll(io.LimitReader(resp.Body,8<<20))
		resp.Body.Close()
		if readErr!=nil {result.Err=readErr;continue}
		if resp.StatusCode<200||resp.StatusCode>=300 {result.Err=fmt.Errorf("model %s returned HTTP %d",model,resp.StatusCode);return result}
		result.Text=extractChatText(body)
		result.Usage=extractFusionUsage(body)
		result.CostUSD,result.CostKnown=estimateRequestCost(provider.config,target.UpstreamModel,result.Usage.PromptTokens,result.Usage.CompletionTokens)
		if result.Text=="" {result.Err=fmt.Errorf("model %s returned an empty answer",model)}
		return result
	}
	if result.Err==nil { result.Err=errors.New("no healthy credentials available") }
	return result
}

func (s *Server) handleFusionChat(w http.ResponseWriter, r *http.Request, payload map[string]json.RawMessage, comboName string, route config.Route, state adminState, providers map[string]config.Provider, record *requestRecord, saverMode, codeMode string) {
	timeout:=time.Duration(route.FusionTimeoutSeconds)*time.Second
	if timeout<=0 { timeout=45*time.Second }
	ctx,cancel:=context.WithTimeout(r.Context(),timeout)
	defer cancel()
	panelPayload:=clonePayloadWithoutTools(payload)
	flattenFusionMessages(panelPayload)
	results:=make(chan fusionPanelResult,len(route.FusionModels))
	var wg sync.WaitGroup
	for _,model:=range route.FusionModels {
		model:=model
		wg.Add(1)
		go func(){
			defer wg.Done()
			results<-s.requestFusionModel(ctx,r,panelPayload,model,state,providers,saverMode,codeMode)
		}()
	}
	go func(){wg.Wait();close(results)}()
	successful:=make([]fusionPanelResult,0,len(route.FusionModels))
	attempts:=0
	for result:=range results {
		attempts+=result.Attempts
		if result.Err==nil && result.Text!="" {successful=append(successful,result)}
	}
	w.Header().Set("X-Fantom-Fusion-Panel-Size",fmt.Sprint(len(route.FusionModels)))
	w.Header().Set("X-Fantom-Fusion-Successes",fmt.Sprint(len(successful)))
	w.Header().Set("X-Fantom-Upstream-Attempts",fmt.Sprint(attempts))
	if len(successful)==0 || (len(successful)<route.FusionMinPanel && len(successful)>1) {
		writeAPIError(w,http.StatusServiceUnavailable,"Fusion panel did not reach its configured quorum","provider_error")
		record.Status=http.StatusServiceUnavailable
		record.Engine="fusion"
		return
	}
	totalPrompt,totalCompletion,totalTokens:=int64(0),int64(0),int64(0)
	totalCost:=float64(0)
	costKnown:=true
	for _,item:=range successful {
		totalPrompt+=item.Usage.PromptTokens
		totalCompletion+=item.Usage.CompletionTokens
		totalTokens+=item.Usage.TotalTokens
		if item.CostKnown { totalCost+=item.CostUSD } else { costKnown=false }
	}
	finalText:=""
	judgeUsed:=""
	if len(successful)==1 {
		finalText=successful[0].Text
		judgeUsed="single-panel-degraded"
	} else {
		judgeModel:=strings.TrimSpace(route.JudgeModel)
		if judgeModel=="" {judgeModel=route.FusionModels[0]}
		var sourceBlocks []string
		for index,item:=range successful {
			sourceBlocks=append(sourceBlocks,fmt.Sprintf("Source %d:\n%s",index+1,item.Text))
		}
		prompt:="You are the judge in a model-fusion panel. Several expert models independently answered the user's request. Their responses are anonymized below. Identify agreement, resolve contradictions, correct factual or reasoning errors, account for important omissions, and produce one coherent final answer. Do not mention model names or source labels.\n\n"+strings.Join(sourceBlocks,"\n\n")
		var messages []map[string]any
		if raw:=payload["messages"];len(raw)>0 {_=json.Unmarshal(raw,&messages)}
		messages=append(messages,map[string]any{"role":"user","content":prompt})
		judgePayload:=clonePayloadWithoutTools(payload)
		encodedMessages,_:=json.Marshal(messages)
		judgePayload["messages"]=encodedMessages
		judgePayload["model"],_=json.Marshal(judgeModel)
		judgePayload["stream"]=json.RawMessage("false")
		judgeResult:=s.requestFusionModel(ctx,r,judgePayload,judgeModel,state,providers,"off","off")
		attempts+=judgeResult.Attempts
		if judgeResult.Err==nil&&judgeResult.Text!="" {
			finalText=judgeResult.Text
			totalPrompt+=judgeResult.Usage.PromptTokens
			totalCompletion+=judgeResult.Usage.CompletionTokens
			totalTokens+=judgeResult.Usage.TotalTokens
			if judgeResult.CostKnown { totalCost+=judgeResult.CostUSD } else { costKnown=false }
			judgeUsed=judgeModel
		} else {
			finalText=successful[0].Text
			judgeUsed="judge-failed-degraded"
		}
	}
	record.Engine="fusion"
	record.Provider="fusion"
	record.Model=comboName
	record.UpstreamAttempts=attempts
	record.PromptTokens=totalPrompt
	record.CompletionTokens=totalCompletion
	record.TotalTokens=totalTokens
	if costKnown { record.CostUSD=totalCost; record.CostKnown=true }
	w.Header().Set("X-Fantom-Fusion-Judge",judgeUsed)
	w.Header().Set("X-Fantom-Fusion-Panel-Successes",fmt.Sprint(len(successful)))
	w.Header().Set("Content-Type","application/json; charset=utf-8")
	if string(payload["stream"])=="true" {
		writeFusionSSE(w,comboName,finalText,totalPrompt,totalCompletion,totalTokens)
		return
	}
	writeFusionJSON(w,comboName,finalText,totalPrompt,totalCompletion,totalTokens)
}

func writeFusionJSON(w http.ResponseWriter, model,text string,prompt,completion,total int64) {
	writeJSON(w,http.StatusOK,map[string]any{
		"id":fmt.Sprintf("chatcmpl-fusion-%d",time.Now().UnixNano()),
		"object":"chat.completion","created":time.Now().Unix(),"model":model,
		"choices":[]any{map[string]any{"index":0,"message":map[string]any{"role":"assistant","content":text},"finish_reason":"stop"}},
		"usage":map[string]any{"prompt_tokens":prompt,"completion_tokens":completion,"total_tokens":total},
	})
}

func writeFusionSSE(w http.ResponseWriter,model,text string,prompt,completion,total int64) {
	w.Header().Set("Content-Type","text/event-stream")
	w.Header().Set("Cache-Control","no-cache")
	w.Header().Set("Connection","keep-alive")
	id:=fmt.Sprintf("chatcmpl-fusion-%d",time.Now().UnixNano())
	created:=time.Now().Unix()
	write:=func(value map[string]any) {
		raw,_:=json.Marshal(value)
		_,_=fmt.Fprintf(w,"data: %s\n\n",raw)
		if flusher,ok:=w.(http.Flusher);ok {flusher.Flush()}
	}
	write(map[string]any{"id":id,"object":"chat.completion.chunk","created":created,"model":model,"choices":[]any{map[string]any{"index":0,"delta":map[string]any{"role":"assistant","content":text},"finish_reason":nil}}})
	write(map[string]any{"id":id,"object":"chat.completion.chunk","created":created,"model":model,"choices":[]any{map[string]any{"index":0,"delta":map[string]any{},"finish_reason":"stop"}}})
	_,_=fmt.Fprintf(w,"data: [DONE]\n\n")
	if flusher,ok:=w.(http.Flusher);ok {flusher.Flush()}
}

func estimateRequestCost(provider config.Provider, model string, promptTokens, completionTokens int64) (float64,bool) {
	pricing,ok:=provider.ModelPricing[model]
	if !ok {return 0,false}
	cost:=float64(promptTokens)*pricing.InputPerMillionUSD/1_000_000 + float64(completionTokens)*pricing.OutputPerMillionUSD/1_000_000
	return cost,true
}
