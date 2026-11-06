package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/mojitex/fantom/internal/config"
)

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	parts:=strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path,"/api/admin/"),"/"),"/")

	if len(parts)==2 && parts[0]=="models" && parts[1]=="refresh" {
		if r.Method!=http.MethodPost { methodNotAllowed(w); return }
		for _,saved:=range s.store.snapshot().Providers {
			if runtime:=s.providerByID(saved.ID); runtime!=nil {
				runtime.catalogMu.Lock()
				runtime.catalogFetchedAt=time.Time{}
				runtime.catalogMu.Unlock()
			}
		}
		coreSyncErr := s.syncCoreProviders()
		result := map[string]any{"ok":true,"message":"Provider model catalogs invalidated and mirrored to the embedded router","core_sync_ok":coreSyncErr==nil}
		if coreSyncErr!=nil { result["core_sync_error"]=coreSyncErr.Error() }
		writeJSON(w,http.StatusOK,result)
		return
	}
	if len(parts)==1 && parts[0]=="capacity-adapters" {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, map[string]any{"adapters": s.store.snapshot().CapacityAdapters})
		case http.MethodPut:
			var body struct { Adapters map[string]capacityAdapterPool `json:"adapters"` }
			if !decodeAdminJSON(w, r, &body) { return }
			if body.Adapters == nil { body.Adapters = map[string]capacityAdapterPool{} }
			if err := s.store.setCapacityAdapters(body.Adapters); err != nil { adminError(w, http.StatusBadRequest, err.Error()); return }
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "adapters": s.store.snapshot().CapacityAdapters})
		default: methodNotAllowed(w)
		}
		return
	}
	if len(parts)==1 && parts[0]=="providers" {
		switch r.Method {
		case http.MethodGet: s.listProviders(w,r)
		case http.MethodPost: s.saveProvider(w,r,"")
		default: methodNotAllowed(w)
		}
		return
	}
	if len(parts)>=2 && parts[0]=="providers" {
		id:=parts[1]
		if len(parts)==3 && parts[2]=="test" && r.Method==http.MethodPost { s.testProvider(w,r,id); return }
		if len(parts)==2 {
			switch r.Method {
			case http.MethodPut, http.MethodPatch: s.saveProvider(w,r,id)
			case http.MethodDelete:
				if err:=s.store.deleteProvider(id); err!=nil { adminError(w,http.StatusConflict,err.Error()); return }
				s.reloadProviderRuntime()
				coreSyncErr := s.syncCoreProviders()
				result := map[string]any{"ok":true,"core_sync_ok":coreSyncErr==nil}
				if coreSyncErr!=nil { result["core_sync_error"]=coreSyncErr.Error(); s.logger.Warn("provider deleted but embedded router sync failed","provider",id,"error",coreSyncErr) }
				writeJSON(w,http.StatusOK,result)
			default: methodNotAllowed(w)
			}
			return
		}
	}
	if len(parts)==1 {
		switch parts[0] {
		case "bootstrap":
			if r.Method!=http.MethodGet { methodNotAllowed(w); return }
			state:=s.store.snapshot()
			writeJSON(w,http.StatusOK,map[string]any{"service":"Fantom","version":"0.2","status":"online","default_provider":state.DefaultProvider,"providers":providerViews(state.Providers),"routes":state.Routes,"settings":map[string]any{
				"token_saver":state.DefaultTokenSaver,"code_mode":state.DefaultCodeMode,
				"caveman_mode":state.DefaultCavemanMode,
				"headroom_enabled":state.HeadroomEnabled,"headroom_url":state.HeadroomURL,
				"headroom_timeout_seconds":state.HeadroomTimeoutSeconds,
				"headroom_compress_user_messages":state.HeadroomCompressUserMessages,
				"capacity_adapters":state.CapacityAdapters,
			},"keys":s.store.listKeys(),"usage":s.usageSummary(state.Requests),"requests":tailRequests(state.Requests,30)})
			return
		case "routes":
			switch r.Method {
			case http.MethodGet: writeJSON(w,http.StatusOK,map[string]any{"routes":s.store.snapshot().Routes})
			case http.MethodPut:
				var body struct { Routes map[string]config.Route `json:"routes"` }
				if !decodeAdminJSON(w,r,&body) { return }
				if body.Routes==nil { body.Routes=map[string]config.Route{} }
				if err:=s.store.setRoutes(body.Routes); err!=nil { adminError(w,http.StatusBadRequest,err.Error()); return }
				writeJSON(w,http.StatusOK,map[string]any{"ok":true,"routes":body.Routes})
			default: methodNotAllowed(w)
			}
			return
		case "keys":
			switch r.Method {
			case http.MethodGet: writeJSON(w,http.StatusOK,map[string]any{"keys":s.store.listKeys()})
			case http.MethodPost:
				var body struct { Name string `json:"name"` }
				if !decodeAdminJSON(w,r,&body) { return }
				if strings.TrimSpace(body.Name)=="" { body.Name="New API key" }
				created,err:=s.store.createKey(body.Name); if err!=nil { adminError(w,http.StatusInternalServerError,err.Error()); return }
				writeJSON(w,http.StatusCreated,created)
			default: methodNotAllowed(w)
			}
			return
		case "settings":
			switch r.Method {
			case http.MethodGet:
				st:=s.store.snapshot()
				writeJSON(w,http.StatusOK,map[string]any{
					"default_provider":st.DefaultProvider,"token_saver":st.DefaultTokenSaver,"code_mode":st.DefaultCodeMode,
					"caveman_mode":st.DefaultCavemanMode,"headroom_enabled":st.HeadroomEnabled,
					"headroom_url":st.HeadroomURL,"headroom_timeout_seconds":st.HeadroomTimeoutSeconds,
					"headroom_compress_user_messages":st.HeadroomCompressUserMessages,
				})
			case http.MethodPut:
				var body struct {
					DefaultProvider string `json:"default_provider"`
					TokenSaver string `json:"token_saver"`
					CodeMode string `json:"code_mode"`
					CavemanMode *string `json:"caveman_mode"`
					HeadroomEnabled *bool `json:"headroom_enabled"`
					HeadroomURL *string `json:"headroom_url"`
					HeadroomTimeoutSeconds *int `json:"headroom_timeout_seconds"`
					HeadroomCompressUserMessages *bool `json:"headroom_compress_user_messages"`
				}
				if !decodeAdminJSON(w,r,&body) { return }
				current:=s.store.snapshot()
				caveman:=current.DefaultCavemanMode
				if body.CavemanMode!=nil { caveman=*body.CavemanMode }
				headroomEnabled:=current.HeadroomEnabled
				if body.HeadroomEnabled!=nil { headroomEnabled=*body.HeadroomEnabled }
				headroomURL:=current.HeadroomURL
				if body.HeadroomURL!=nil { headroomURL=*body.HeadroomURL }
				headroomTimeout:=current.HeadroomTimeoutSeconds
				if body.HeadroomTimeoutSeconds!=nil { headroomTimeout=*body.HeadroomTimeoutSeconds }
				compressUsers:=current.HeadroomCompressUserMessages
				if body.HeadroomCompressUserMessages!=nil { compressUsers=*body.HeadroomCompressUserMessages }
				if err:=s.store.setAdvancedSettings(body.DefaultProvider,body.TokenSaver,body.CodeMode,caveman,headroomEnabled,headroomURL,headroomTimeout,compressUsers); err!=nil {
					adminError(w,http.StatusBadRequest,err.Error()); return
				}
				writeJSON(w,http.StatusOK,map[string]any{"ok":true,"settings":s.store.snapshot()})
			default: methodNotAllowed(w)
			}
			return
		case "logs":
			if r.Method!=http.MethodGet { methodNotAllowed(w); return }
			st:=s.store.snapshot(); limit:=100
			if n:=r.URL.Query().Get("limit"); n!="" { var parsed int; if _,err:=fmt.Sscanf(n,"%d",&parsed); err==nil && parsed>0 && parsed<1000 { limit=parsed } }
			writeJSON(w,http.StatusOK,map[string]any{"requests":tailRequests(st.Requests,limit)})
			return
		case "usage":
			if r.Method!=http.MethodGet { methodNotAllowed(w); return }
			writeJSON(w,http.StatusOK,s.usageSummary(s.store.snapshot().Requests))
			return
		}
	}
	if len(parts)==2 && parts[0]=="keys" && r.Method==http.MethodDelete {
		if err:=s.store.revokeKey(parts[1]); err!=nil { adminError(w,http.StatusNotFound,"API key not found"); return }
		writeJSON(w,http.StatusOK,map[string]any{"ok":true}); return
	}
	adminError(w,http.StatusNotFound,"admin endpoint not found")
}

func decodeAdminJSON(w http.ResponseWriter,r *http.Request,dst any) bool {
	r.Body=http.MaxBytesReader(w,r.Body,1<<20)
	defer r.Body.Close()
	dec:=json.NewDecoder(r.Body); dec.DisallowUnknownFields()
	if err:=dec.Decode(dst); err!=nil { adminError(w,http.StatusBadRequest,"invalid JSON: "+err.Error()); return false }
	return true
}
func adminError(w http.ResponseWriter,status int,message string) { writeJSON(w,status,map[string]any{"error":map[string]string{"message":message}}) }
func methodNotAllowed(w http.ResponseWriter) { adminError(w,http.StatusMethodNotAllowed,"method not allowed") }

func providerViews(items []persistedProvider) []map[string]any {
	out:=make([]map[string]any,0,len(items))
	for _,p:=range sortedProviders(items) {
		preview:=""; if len(p.APIKeys)>0 { last:=p.APIKeys[0]; if len(last)>4 { preview="••••"+last[len(last)-4:] } else { preview="••••" } }
		out=append(out,map[string]any{"id":p.ID,"base_url":p.BaseURL,"chat_path":p.ChatPath,"models_url":p.ModelsURL,"auth_type":p.AuthType,"models":p.Models,"timeout_seconds":p.TimeoutSeconds,"has_api_key":len(p.APIKeys)>0,"api_key_preview":preview,"key_count":len(p.APIKeys),"model_count":len(p.Models),"model_capabilities":p.ModelCapabilities,"model_pricing":p.ModelPricing})
	}
	return out
}

func (s *Server) listProviders(w http.ResponseWriter,r *http.Request) {
	st:=s.store.snapshot(); writeJSON(w,http.StatusOK,map[string]any{"providers":providerViews(st.Providers),"default_provider":st.DefaultProvider})
}
func (s *Server) saveProvider(w http.ResponseWriter,r *http.Request,id string) {
	var body providerInput
	if !decodeAdminJSON(w,r,&body) { return }
	st:=s.store.snapshot()
	var existing *config.Provider
	for _,saved:=range st.Providers { p:=saved.runtime(); if p.ID==id || (id=="" && p.ID==strings.ToLower(strings.TrimSpace(body.ID))) { existing=&p; break } }
	if id!="" { body.ID=id }
	provider,err:=s.store.appendProviderFromInput(body,existing)
	if err!=nil { adminError(w,http.StatusBadRequest,err.Error()); return }
	if existing!=nil && provider.ID!=existing.ID { adminError(w,http.StatusBadRequest,"provider ID cannot be changed"); return }
	if err:=s.store.replaceProvider(provider); err!=nil { adminError(w,http.StatusInternalServerError,err.Error()); return }
	s.reloadProviderRuntime()
	coreSyncErr := s.syncCoreProviders()
	latest:=s.store.snapshot()
	for _,item:=range latest.Providers {
		if item.ID==provider.ID {
			result := map[string]any{"ok":true,"provider":providerViews([]persistedProvider{item})[0],"core_sync_ok":coreSyncErr==nil}
			if coreSyncErr!=nil { result["core_sync_error"]=coreSyncErr.Error(); s.logger.Warn("provider saved but embedded router sync failed","provider",provider.ID,"error",coreSyncErr) }
			writeJSON(w,http.StatusOK,result)
			return
		}
	}
	adminError(w,http.StatusInternalServerError,"provider saved but could not be reloaded")
}
func (s *Server) testProvider(w http.ResponseWriter,r *http.Request,id string) {
	var target *providerRuntime
	for _,saved:=range s.store.snapshot().Providers {
		if saved.ID==id { p:=saved.runtime(); target=newProviderRuntime(p); break }
	}
	if target==nil { adminError(w,http.StatusNotFound,"provider not found"); return }

	var input struct { Model string `json:"model"` }
	if r.Body!=nil {
		r.Body=http.MaxBytesReader(w,r.Body,16<<10)
		_ = json.NewDecoder(r.Body).Decode(&input)
	}
	model:=strings.TrimSpace(input.Model)
	if model=="" {
		for _,candidate:=range target.config.Models {
			if strings.TrimSpace(candidate)!="" && candidate!="*" { model=strings.TrimSpace(candidate); break }
		}
	}
	catalogOK:=false
	catalogStatus:=0
	modelsURL:=target.config.ModelsURL
	if modelsURL=="" { modelsURL=target.config.BaseURL+"/models" }
	for _,key:=range target.apiKeysForRequest() {
		if target.config.AuthType=="bearer"&&key=="" { break }
		catalogRequest,err:=http.NewRequestWithContext(r.Context(),http.MethodGet,modelsURL,nil)
		if err!=nil { break }
		s.applyProviderHeaders(catalogRequest,target,key)
		catalogResponse,err:=target.client.Do(catalogRequest)
		if err!=nil { continue }
		catalogStatus=catalogResponse.StatusCode
		if catalogStatus>=200&&catalogStatus<300 {
			catalogOK=true
			if model=="" {
				var remote upstreamModelList
				if json.NewDecoder(io.LimitReader(catalogResponse.Body,8<<20)).Decode(&remote)==nil {
					for _,candidate:=range remote.Data {
						if candidate.ID!=""&&modelAllowed(target.config.Models,candidate.ID) { model=candidate.ID; break }
					}
				}
			}
		}
		_ = catalogResponse.Body.Close()
		if catalogOK || !shouldRetryCredential(catalogStatus) { break }
	}
	if model=="" {
		writeJSON(w,http.StatusOK,map[string]any{
			"ok":false,"provider":id,"catalog_ok":catalogOK,"catalog_status":catalogStatus,
			"error":"The model catalog did not provide a testable model. Configure an explicit model ID or supply a model in the test request.",
			"tested_at":time.Now().UTC(),
		})
		return
	}

	payload,_:=json.Marshal(map[string]any{
		"model":model,
		"messages":[]map[string]string{{"role":"user","content":"Reply with OK."}},
		"stream":false,
	})
	started:=time.Now()
	var chatStatus int
	var chatError string
	for _,key:=range target.apiKeysForRequest() {
		if target.config.AuthType=="bearer"&&key=="" { chatError="provider has no API key"; break }
		chatRequest,err:=http.NewRequestWithContext(r.Context(),http.MethodPost,target.config.BaseURL+target.config.ChatPath,bytes.NewReader(payload))
		if err!=nil { chatError=err.Error(); break }
		chatRequest.Header.Set("Content-Type","application/json")
		chatRequest.Header.Set("Accept","application/json")
		s.applyProviderHeaders(chatRequest,target,key)
		chatResponse,err:=target.client.Do(chatRequest)
		if err!=nil { chatError=err.Error(); continue }
		chatStatus=chatResponse.StatusCode
		_,_=io.Copy(io.Discard,io.LimitReader(chatResponse.Body,1<<20))
		_ = chatResponse.Body.Close()
		if chatStatus>=200&&chatStatus<300 { chatError=""; break }
		chatError=http.StatusText(chatStatus)
		if !shouldRetryCredential(chatStatus) { break }
	}
	latency:=time.Since(started).Milliseconds()
	ok:=chatStatus>=200&&chatStatus<300
	writeJSON(w,http.StatusOK,map[string]any{
		"ok":ok,"provider":id,"catalog_ok":catalogOK,"catalog_status":catalogStatus,
		"chat_ok":ok,"chat_status":chatStatus,"test_model":model,"latency_ms":latency,
		"error":chatError,"tested_at":time.Now().UTC(),
	})
}
func tailRequests(items []requestRecord,limit int) []requestRecord {
	if limit>len(items) { limit=len(items) }
	out:=append([]requestRecord(nil),items[len(items)-limit:]...)
	sort.Slice(out,func(i,j int)bool{return out[i].At.After(out[j].At)})
	return out
}
func (s *Server) usageSummary(items []requestRecord) map[string]any {
	var total,success,failed int
	var latency,promptTokens,completionTokens,totalTokens int64
	var estimatedCost float64
	pricedRequests := 0
	providers:=map[string]int{}
	models:=map[string]int{}
	for _,item:=range items {
		total++; latency+=item.LatencyMS
		promptTokens+=item.PromptTokens
		completionTokens+=item.CompletionTokens
		totalTokens+=item.TotalTokens
		if item.CostKnown { estimatedCost += item.CostUSD; pricedRequests++ }
		providers[item.Provider]++; models[item.Model]++
		if item.Status>=200&&item.Status<400 { success++ } else { failed++ }
	}
	avg:=int64(0); if total>0 { avg=latency/int64(total) }
	coverage := 0.0
	if total > 0 { coverage = float64(pricedRequests) / float64(total) * 100 }
	return map[string]any{
		"total_requests":total,"successful_requests":success,"failed_requests":failed,"avg_latency_ms":avg,
		"total_prompt_tokens":promptTokens,"total_completion_tokens":completionTokens,"total_tokens":totalTokens,
		"total_estimated_cost_usd":estimatedCost,"priced_requests":pricedRequests,"unpriced_requests":total-pricedRequests,"cost_coverage_percent":coverage,
		"by_provider":providers,"by_model":models,
	}
}

var _ = errors.New
