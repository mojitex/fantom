"use client";

import { useCallback, useEffect, useState } from "react";
import type { FormEvent, ReactNode } from "react";

type Page = "endpoint" | "providers" | "combos" | "usage" | "settings";
type Provider = { id:string; base_url:string; chat_path:string; models_url?:string; auth_type:string; models:string[]; timeout_seconds:number; has_api_key:boolean; api_key_preview?:string; key_count?:number };
type Model = { id:string; owned_by?:string };
type Route = { provider:string; upstream_model:string; fallback_models?:string[] };
type Key = { id:string; name:string; prefix:string; enabled:boolean; created_at:string };
type Log = { id:string; at:string; provider:string; model:string; status:number; latency_ms:number; streaming:boolean; prompt_tokens?:number; completion_tokens?:number; total_tokens?:number; upstream_attempts?:number; credential_retries?:number; fallback_attempts?:number };
type Usage = { total_requests:number; successful_requests:number; failed_requests:number; avg_latency_ms:number; total_prompt_tokens?:number; total_completion_tokens?:number; total_tokens?:number; by_provider:Record<string,number>; by_model:Record<string,number> };
type Snapshot = { service:string; version:string; status:string; default_provider:string; providers:Provider[]; routes:Record<string,Route>; settings:{token_saver:string;code_mode:string}; keys:Key[]; usage:Usage; requests:Log[] };

const emptyUsage:Usage={total_requests:0,successful_requests:0,failed_requests:0,avg_latency_ms:0,by_provider:{},by_model:{}};
const nav:{id:Page;label:string;glyph:string}[]=[
 {id:"endpoint",label:"Endpoint & Key",glyph:"key"},
 {id:"providers",label:"Providers",glyph:"layers"},
 {id:"combos",label:"Combos",glyph:"route"},
 {id:"usage",label:"Usage",glyph:"chart"},
];

function Icon({name,size=18}:{name:string;size?:number}) {
 const paths:Record<string,ReactNode>={
  grid:<><rect x="3" y="3" width="7" height="7" rx="1.5"/><rect x="14" y="3" width="7" height="7" rx="1.5"/><rect x="3" y="14" width="7" height="7" rx="1.5"/><rect x="14" y="14" width="7" height="7" rx="1.5"/></>,
  layers:<><path d="m12 3 9 5-9 5-9-5 9-5Z"/><path d="m3 12 9 5 9-5M3 16l9 5 9-5"/></>,
  cpu:<><rect x="5" y="5" width="14" height="14" rx="2"/><path d="M9 9h6v6H9zM9 1v4m6-4v4M9 19v4m6-4v4M1 9h4m-4 6h4m14-6h4m-4 6h4"/></>,
  route:<><circle cx="6" cy="6" r="3"/><circle cx="18" cy="18" r="3"/><path d="M9 6h4a5 5 0 0 1 5 5v4M15 18h-4a5 5 0 0 1-5-5V9"/></>,
  key:<><circle cx="8" cy="15" r="5"/><path d="m11.5 11.5 9-9M17 6l2 2m-5 1 2 2"/></>,
  chart:<><path d="M3 3v18h18"/><path d="m7 14 4-4 4 3 6-7"/></>,
  terminal:<><path d="m4 17 6-6-6-6m8 14h8"/></>,
  settings:<><circle cx="12" cy="12" r="3"/><path d="m19.4 15 .1.1 1.4 1.1-1.4 2.4-1.7-.7a8 8 0 0 1-1.7 1l-.3 1.8h-2.8l-.3-1.8a8 8 0 0 1-1.7-1l-1.7.7-1.4-2.4L7.3 15a8 8 0 0 1 0-2l-1.4-1.1 1.4-2.4 1.7.7a8 8 0 0 1 1.7-1l.3-1.8h2.8l.3 1.8a8 8 0 0 1 1.7 1l1.7-.7 1.4 2.4-1.4 1.1a8 8 0 0 1-.1 2Z"/></>,
  plus:<path d="M12 5v14m-7-7h14"/>,
  refresh:<><path d="M20 7v5h-5"/><path d="M4 17v-5h5"/><path d="M5.6 9A7 7 0 0 1 18 6l2 6M4 12l2 6a7 7 0 0 0 12.4-3"/></>,
  search:<><circle cx="11" cy="11" r="7"/><path d="m20 20-4-4"/></>,
  check:<path d="m5 12 4 4L19 6"/>,
  alert:<><path d="M12 3 2.7 19a2 2 0 0 0 1.7 3h15.2a2 2 0 0 0 1.7-3L12 3Z"/><path d="M12 9v4m0 4h.01"/></>,
  external:<><path d="M14 3h7v7m-1-6-9 9"/><path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6"/></>,
  copy:<><rect x="8" y="8" width="12" height="12" rx="2"/><path d="M16 8V5a2 2 0 0 0-2-2H5a2 2 0 0 0-2 2v9a2 2 0 0 0 2 2h3"/></>,
  trash:<><path d="M3 6h18m-2 0-1 14H6L5 6m3 0V4h8v2m-6 4v6m4-6v6"/></>,
  logout:<><path d="M10 17l5-5-5-5m5 5H3"/><path d="M12 3h7a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2h-7"/></>,
  zap:<path d="m13 2-3 9h8L9 22l3-9H4l9-11Z"/>,
  shield:<><path d="M12 22s8-4 8-11V5l-8-3-8 3v6c0 7 8 11 8 11Z"/><path d="m9 12 2 2 4-4"/></>,
 };
 return <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">{paths[name]||paths.grid}</svg>;
}

function formatNumber(value:number){return new Intl.NumberFormat("en-US").format(value||0)}
function ago(value:string){if(!value)return "—";const n=Date.now()-new Date(value).getTime();if(!Number.isFinite(n))return "—";if(n<60000)return "just now";if(n<3600000)return Math.floor(n/60000)+"m ago";if(n<86400000)return Math.floor(n/3600000)+"h ago";return Math.floor(n/86400000)+"d ago"}
function cls(...items:(string|false|undefined|null)[]){return items.filter(Boolean).join(" ")}

export default function Home(){
 const [page,setPage]=useState<Page>("endpoint");
 const [apiKey,setApiKey]=useState("");
 const [authenticated,setAuthenticated]=useState(false);
 const [booting,setBooting]=useState(true);
 const [busy,setBusy]=useState(false);
 const [error,setError]=useState("");
 const [notice,setNotice]=useState("");
 const [snapshot,setSnapshot]=useState<Snapshot|null>(null);
 const [models,setModels]=useState<Model[]>([]);
 const [showProviderForm,setShowProviderForm]=useState(false);
 const [editingProvider,setEditingProvider]=useState<Provider|null>(null);
 const [newKey,setNewKey]=useState("");
 const [keyName,setKeyName]=useState("My application");
 const [alias,setAlias]=useState("");
 const [routeProvider,setRouteProvider]=useState("");
 const [upstreamModel,setUpstreamModel]=useState("");
 const [fallbackModels,setFallbackModels]=useState("");
 const [providerForm,setProviderForm]=useState({id:"",base_url:"",chat_path:"/chat/completions",models_url:"",auth_type:"bearer",api_keys:"",models:"*",timeout_seconds:"100"});
 const [settings,setSettings]=useState({default_provider:"nvidia",token_saver:"safe",code_mode:"off"});

 const request=useCallback(async(path:string,init:RequestInit={})=>{
  const headers=new Headers(init.headers||{});headers.set("Authorization","Bearer "+apiKey);if(init.body)headers.set("Content-Type","application/json");
  const response=await fetch(path,{...init,headers,cache:"no-store"});
  const data=await response.json().catch(()=>({}));
  if(!response.ok)throw new Error(data?.error?.message||data?.error||("Request failed ("+response.status+")"));
  return data;
 },[apiKey]);

 const load=useCallback(async()=>{
  setBusy(true);setError("");
  try{
   const data=await request("/api/admin/bootstrap") as Snapshot;
   setSnapshot(data);setSettings({default_provider:data.default_provider||data.providers?.[0]?.id||"",token_saver:data.settings?.token_saver||"safe",code_mode:data.settings?.code_mode||"off"});
   setAuthenticated(true);sessionStorage.setItem("fantom-admin-key",apiKey);
   const [modelData,logData]=await Promise.all([request("/v1/models").catch(()=>({data:[]})),request("/api/admin/logs?limit=150").catch(()=>({requests:data.requests||[]}))]);
   setModels(Array.isArray(modelData.data)?modelData.data:[]);
   setSnapshot(prev=>prev?{...prev,requests:logData.requests||prev.requests}:prev);
  }catch(e){setAuthenticated(false);setError(e instanceof Error?e.message:"Unable to connect to Fantom");}
  finally{setBusy(false);setBooting(false);}
 },[apiKey,request]);

 useEffect(()=>{const saved=sessionStorage.getItem("fantom-admin-key");if(saved){setApiKey(saved);}setBooting(false);},[]);
 useEffect(()=>{if(apiKey&& !authenticated && !booting && sessionStorage.getItem("fantom-admin-key")===apiKey){void load();}},[apiKey,authenticated,booting,load]);

 async function action(fn:()=>Promise<unknown>,success:string){setBusy(true);setError("");setNotice("");try{await fn();setNotice(success);await load();}catch(e){setError(e instanceof Error?e.message:"Operation failed");}finally{setBusy(false);}}
 function resetProvider(){setEditingProvider(null);setProviderForm({id:"",base_url:"",chat_path:"/chat/completions",models_url:"",auth_type:"bearer",api_keys:"",models:"*",timeout_seconds:"100"});setShowProviderForm(true);}
 function editProvider(p:Provider){setEditingProvider(p);setProviderForm({id:p.id,base_url:p.base_url,chat_path:p.chat_path||"/chat/completions",models_url:p.models_url||"",auth_type:p.auth_type,api_keys:"",models:(p.models||[]).join(", "),timeout_seconds:String(p.timeout_seconds||100)});setShowProviderForm(true);}
 async function saveProvider(e:FormEvent){e.preventDefault();const body={...providerForm,api_keys:providerForm.api_keys.split(/\r?\n|,/).map(x=>x.trim()).filter(Boolean),models:providerForm.models.split(",").map(x=>x.trim()).filter(Boolean),timeout_seconds:Number(providerForm.timeout_seconds)};await action(async()=>{await request(editingProvider?"/api/admin/providers/"+encodeURIComponent(editingProvider.id):"/api/admin/providers",{method:editingProvider?"PUT":"POST",body:JSON.stringify(body)});setShowProviderForm(false);setEditingProvider(null);},"Provider saved and runtime reloaded");}
 async function testProvider(id:string){setBusy(true);setError("");try{const data=await request("/api/admin/providers/"+encodeURIComponent(id)+"/test",{method:"POST",body:"{}"});setNotice(data.ok?"Chat test passed · "+data.test_model+" · "+data.latency_ms+" ms":"Chat test failed · "+(data.error||data.message)+" · HTTP "+(data.chat_status||data.http_status||"—"));}catch(e){setError(e instanceof Error?e.message:"Connection test failed");}finally{setBusy(false);}}
 async function refreshModelCatalog(){setBusy(true);setError("");try{await request("/api/admin/models/refresh",{method:"POST"});await load();}catch(e){setError(e instanceof Error?e.message:"Could not refresh model catalog");}finally{setBusy(false);}}
 async function saveRoute(e:React.FormEvent){e.preventDefault();const next={...(snapshot?.routes||{}),[alias]:{provider:routeProvider,upstream_model:upstreamModel,fallback_models:fallbackModels.split(",").map(x=>x.trim()).filter(Boolean)}};await action(async()=>{await request("/api/admin/routes",{method:"PUT",body:JSON.stringify({routes:next})});setAlias("");setUpstreamModel("");setFallbackModels("");},"Combo saved");}
 async function removeRoute(name:string){const next={...(snapshot?.routes||{})};delete next[name];await action(()=>request("/api/admin/routes",{method:"PUT",body:JSON.stringify({routes:next})}),"Route removed");}
 async function createKey(e:React.FormEvent){e.preventDefault();setBusy(true);setError("");try{const data=await request("/api/admin/keys",{method:"POST",body:JSON.stringify({name:keyName})});setNewKey(data.key);setNotice("API key created. Copy it now; it will not be shown again.");await load();}catch(e){setError(e instanceof Error?e.message:"Could not create key");}finally{setBusy(false);}}
 async function saveSettings(e:React.FormEvent){e.preventDefault();await action(()=>request("/api/admin/settings",{method:"PUT",body:JSON.stringify(settings)}),"Gateway settings saved");}
 function signOut(){sessionStorage.removeItem("fantom-admin-key");setApiKey("");setAuthenticated(false);setSnapshot(null);setModels([]);}

 const usage=snapshot?.usage||emptyUsage;
 const activeNav=nav.find(n=>n.id===page)||{id:"settings" as Page,label:"Settings",glyph:"settings"};

 if(booting)return <main className="boot-screen"><div className="brand-symbol">F</div><span>Starting Fantom control plane…</span></main>;
 if(!authenticated)return <main className="login-screen"><div className="login-glow"/><div className="login-card"><div className="login-brand"><div className="brand-symbol">F</div><div><b>FANTOM</b><span>AI ROUTING CONTROL PLANE</span></div></div><div className="login-eyebrow"><i/> SELF-HOSTED GATEWAY</div><h1>Control your<br/><span>AI infrastructure.</span></h1><p>Sign in with your gateway administration key to manage providers, routes, credentials and traffic.</p><form onSubmit={e=>{e.preventDefault();void load();}}><label>GATEWAY ADMIN KEY</label><input autoFocus type="password" value={apiKey} onChange={e=>setApiKey(e.target.value)} placeholder="ftm_live_… or GATEWAY_API_KEY"/><button className="primary wide" disabled={!apiKey.trim()||busy}>{busy?"AUTHENTICATING…":"OPEN CONTROL PLANE"} <Icon name="external" size={15}/></button></form>{error&&<div className="alert error"><Icon name="alert"/>{error}</div>}<div className="login-foot"><Icon name="shield" size={14}/> Credentials stay in this browser session</div></div><div className="login-version">FANTOM / 0.2 CONTROL PLANE</div></main>;

 return <div className="app-shell">
  <aside className="sidebar">
   <div className="sidebar-brand"><div className="brand-symbol">F</div><div><b>FANTOM</b><span>ROUTING ENGINE</span></div><span className="version-tag">0.2</span></div>
   <nav className="simple-nav">{nav.map(n=><button key={n.id} className={cls("nav-link",page===n.id&&"selected")} onClick={()=>{setPage(n.id);setError("");setNotice("");}}><Icon name={n.glyph}/><span>{n.label}</span>{n.id==="providers"&&<small>{snapshot?.providers?.length||0}</small>}</button>)}</nav>
   <div className="sidebar-bottom"><button className={cls("nav-link",page==="settings"&&"selected")} onClick={()=>setPage("settings")}><Icon name="settings"/> <span>Settings</span></button><button className="nav-link signout" onClick={signOut}><Icon name="logout"/> <span>Sign out</span></button><div className="sidebar-meta">FANTOM <span>AI ROUTER</span></div></div>
  </aside>
  <main className="main-area">
   <header className="topbar"><div className="crumb"><span>FANTOM</span><b>/</b>{activeNav.label}</div><div className="top-actions"><button className="icon-button" title="Refresh data" onClick={()=>void load()} disabled={busy}><Icon name="refresh"/></button><div className="user-avatar">M</div></div></header>
   <div className="page-wrap">
    <div className="page-heading"><div><div className="eyebrow">FANTOM / CONTROL PLANE</div><h1>{pageTitle(page)}</h1><p>{pageDescription(page)}</p></div><div className="heading-actions">{page==="providers"&&<button className="primary" onClick={resetProvider}><Icon name="plus" size={16}/> Add provider</button>}</div></div>
    {(error||notice)&&<div className={cls("alert",error?"error":"success")}><Icon name={error?"alert":"check"}/><span>{error||notice}</span><button onClick={()=>{setError("");setNotice("");}}>×</button></div>}
    {page==="endpoint"&&<EndpointPage snapshot={snapshot} newKey={newKey} setNewKey={setNewKey} keyName={keyName} setKeyName={setKeyName} busy={busy} onCreateKey={createKey} onRevokeKey={id=>void action(()=>request("/api/admin/keys/"+id,{method:"DELETE"}),"API key revoked")}/>}
    {page==="providers"&&<Providers providers={snapshot?.providers||[]} models={models} onRefresh={()=>void refreshModelCatalog()} onAdd={resetProvider} onEdit={editProvider} onTest={testProvider} onDelete={id=>void action(()=>request("/api/admin/providers/"+encodeURIComponent(id),{method:"DELETE"}),"Provider deleted")}/>}
    {page==="combos"&&<Routing routes={snapshot?.routes||{}} providers={snapshot?.providers||[]} alias={alias} setAlias={setAlias} routeProvider={routeProvider||snapshot?.providers?.[0]?.id||""} setRouteProvider={setRouteProvider} upstreamModel={upstreamModel} setUpstreamModel={setUpstreamModel} fallbackModels={fallbackModels} setFallbackModels={setFallbackModels} onSave={saveRoute} onRemove={removeRoute}/>}
    {page==="usage"&&<UsagePage usage={usage} requests={snapshot?.requests||[]}/>}
    {page==="settings"&&<form onSubmit={saveSettings} className="settings-grid"><section className="panel settings-panel"><div className="panel-title"><div className="icon-tile violet"><Icon name="route"/></div><div><b>Default routing</b><span>Provider used when a model has no explicit alias.</span></div></div><label>DEFAULT PROVIDER</label><select value={settings.default_provider} onChange={e=>setSettings({...settings,default_provider:e.target.value})}>{(snapshot?.providers||[]).map(p=><option key={p.id} value={p.id}>{p.id}</option>)}</select></section><section className="panel settings-panel"><div className="panel-title"><div className="icon-tile green"><Icon name="zap"/></div><div><b>Token optimization</b><span>Safe terminal-output normalization, applied before upstream calls.</span></div></div><label>DEFAULT TOKEN SAVER</label><select value={settings.token_saver} onChange={e=>setSettings({...settings,token_saver:e.target.value})}><option value="safe">Safe · normalize command output</option><option value="off">Off · pass through unchanged</option></select><label>CODE GENERATION MODE</label><select value={settings.code_mode} onChange={e=>setSettings({...settings,code_mode:e.target.value})}><option value="off">Off</option><option value="lite">Lite</option><option value="full">Full</option><option value="ultra">Ultra</option></select></section><section className="panel settings-panel security-panel"><div className="panel-title"><div className="icon-tile amber"><Icon name="shield"/></div><div><b>Security & persistence</b><span>Admin APIs require a gateway key. State is stored in the mounted data directory.</span></div></div><div className="info-row"><span>Persistent state file</span><code>FANTOM_DATA_DIR/fantom-state.json</code></div><div className="info-row"><span>Credential storage</span><b>Local file · restricted permissions</b></div><div className="info-row"><span>API key handling</span><b>New keys hashed at rest</b></div></section><div className="settings-save"><span>Changes are applied immediately and persisted to disk.</span><button className="primary" disabled={busy}>Save settings <Icon name="check" size={15}/></button></div></form>}
   </div>
   <footer className="app-footer"><span>FANTOM CONTROL PLANE <b>0.2</b></span><span>ONE ENDPOINT. TOTAL CONTROL.</span></footer>
  </main>
  {showProviderForm&&<div className="modal-backdrop" onMouseDown={e=>{if(e.target===e.currentTarget)setShowProviderForm(false);}}><form className="modal" onSubmit={saveProvider}><div className="modal-head"><div><div className="eyebrow">PROVIDER CONFIGURATION</div><h2>{editingProvider?"Edit provider":"Connect a provider"}</h2></div><button type="button" className="icon-button" onClick={()=>setShowProviderForm(false)}>×</button></div><div className="form-grid"><div><label>PROVIDER ID</label><input required disabled={!!editingProvider} value={providerForm.id} onChange={e=>setProviderForm({...providerForm,id:e.target.value})} placeholder="e.g. openrouter"/></div><div><label>AUTHENTICATION</label><select value={providerForm.auth_type} onChange={e=>setProviderForm({...providerForm,auth_type:e.target.value})}><option value="bearer">Bearer API key</option><option value="none">No authentication</option></select></div><div className="span-2"><label>BASE URL</label><input required value={providerForm.base_url} onChange={e=>setProviderForm({...providerForm,base_url:e.target.value})} placeholder="https://api.provider.com/v1"/></div><details className="provider-advanced span-2"><summary>Advanced settings</summary><div className="advanced-fields"><div><label>CHAT COMPLETIONS PATH</label><input value={providerForm.chat_path} onChange={e=>setProviderForm({...providerForm,chat_path:e.target.value})}/></div><div><label>MODELS ENDPOINT (OPTIONAL)</label><input value={providerForm.models_url} onChange={e=>setProviderForm({...providerForm,models_url:e.target.value})} placeholder="https://…/models"/></div></div></details><div className="span-2"><label>API KEY POOL {editingProvider&&"(leave blank to keep current keys)"}</label><textarea required={!editingProvider&&providerForm.auth_type==="bearer"} rows={3} autoComplete="off" value={providerForm.api_keys} onChange={e=>setProviderForm({...providerForm,api_keys:e.target.value})} placeholder={editingProvider?"Add replacement keys, one per line or comma-separated":"Paste one or more API keys, one per line"}/><small>Keys rotate round-robin per upstream request. Existing keys are never shown again.</small></div><details className="provider-advanced span-2"><summary>Model access & timeout</summary><div className="advanced-fields"><div><label>MODEL ALLOWLIST</label><input value={providerForm.models} onChange={e=>setProviderForm({...providerForm,models:e.target.value})} placeholder="* or model-a, model-b"/><small>Use * to expose every model returned by the provider catalog.</small></div><div><label>TIMEOUT (SECONDS)</label><input type="number" min="1" max="600" value={providerForm.timeout_seconds} onChange={e=>setProviderForm({...providerForm,timeout_seconds:e.target.value})}/></div></div></details></div><div className="modal-actions"><button type="button" className="secondary" onClick={()=>setShowProviderForm(false)}>Cancel</button><button className="primary" disabled={busy}>{busy?"SAVING…":"Save provider"}</button></div></form></div>}
  <div className="mobile-nav">{nav.map(n=><button key={n.id} className={page===n.id?"active":""} onClick={()=>setPage(n.id)}><Icon name={n.glyph} size={18}/><span>{n.label}</span></button>)}<button className={page==="settings"?"active":""} onClick={()=>setPage("settings")}><Icon name="settings" size={18}/><span>Settings</span></button></div>
 </div>;
}

function pageTitle(p:Page){return ({endpoint:"Endpoint & Key",providers:"Providers",combos:"Combos",usage:"Usage",settings:"Settings"} as Record<Page,string>)[p]}
function pageDescription(p:Page){return ({endpoint:"Connect your apps to Fantom with one API endpoint and a dedicated key.",providers:"Manage upstream providers, credentials and model discovery.",combos:"Create model aliases and ordered fallback chains.",usage:"Review request volume, model distribution, token usage and request diagnostics.",settings:"Configure defaults and token optimization."} as Record<Page,string>)[p]}

function Metric({label,value,detail,glyph,tone}:{label:string;value:string;detail:string;glyph:string;tone:string}){return <div className="metric-card"><div className="metric-top"><span>{label}</span><div className={"icon-tile "+tone}><Icon name={glyph} size={17}/></div></div><b className="metric-value">{value}</b><small>{detail}</small></div>}
function EndpointPage({snapshot,newKey,setNewKey,keyName,setKeyName,busy,onCreateKey,onRevokeKey}:{snapshot:Snapshot|null;newKey:string;setNewKey:(v:string)=>void;keyName:string;setKeyName:(v:string)=>void;busy:boolean;onCreateKey:(e:FormEvent)=>void;onRevokeKey:(id:string)=>void}) {
 const [origin,setOrigin]=useState("");
 useEffect(()=>{setOrigin(window.location.origin)},[]);
 const baseUrl=(origin||"https://your-domain")+"/v1";
 const copy=(value:string)=>{void navigator.clipboard.writeText(value)};
 const enabled=(snapshot?.keys||[]).filter(k=>k.enabled).length;
 return <div className="endpoint-page">
  <section className="endpoint-card">
   <div className="endpoint-card-top"><div><div className="eyebrow">OPENAI-COMPATIBLE API</div><h2>Your endpoint</h2><p>Use this URL in OpenCode, Codex, Cursor or any compatible client.</p></div><span className="tag green">READY</span></div>
   <div className="endpoint-url"><code>{baseUrl}</code><button className="secondary" onClick={()=>copy(baseUrl)}><Icon name="copy" size={14}/> Copy URL</button></div>
   <div className="endpoint-meta"><span>POST /chat/completions</span><span>GET /models</span><span>Streaming supported</span></div>
  </section>
  <section className="panel endpoint-keys">
   <div className="panel-toolbar"><div><b>API keys</b><span>{enabled} active · create one key per application</span></div><span className="tag">{enabled} ACTIVE</span></div>
   {newKey&&<div className="new-secret"><div><b>Copy your new key</b><span>It will not be shown again.</span></div><code>{newKey}</code><button className="secondary" onClick={()=>copy(newKey)}><Icon name="copy" size={14}/> Copy</button><button className="ghost" onClick={()=>setNewKey("")}>Done</button></div>}
   <form className="inline-create" onSubmit={onCreateKey}><input value={keyName} onChange={e=>setKeyName(e.target.value)} placeholder="Application name"/><button className="primary" disabled={busy}><Icon name="plus" size={14}/> Create key</button></form>
   <div className="table-scroll"><table><thead><tr><th>APPLICATION</th><th>KEY</th><th>CREATED</th><th>STATUS</th><th/></tr></thead><tbody>{(snapshot?.keys||[]).map(k=><tr key={k.id}><td><b>{k.name}</b></td><td><code>{k.prefix}…</code></td><td>{new Date(k.created_at).toLocaleDateString()}</td><td><span className={cls("tag",k.enabled?"green":"red")}>{k.enabled?"ACTIVE":"REVOKED"}</span></td><td className="actions-cell">{k.enabled&&<button className="ghost danger-text" onClick={()=>onRevokeKey(k.id)}><Icon name="trash" size={13}/> Revoke</button>}</td></tr>)}</tbody></table>{(snapshot?.keys||[]).length===0&&<div className="empty">No application keys yet. Create one to connect your first client.</div>}</div>
  </section>
  <section className="panel endpoint-example"><div className="panel-heading"><div><div className="eyebrow">QUICK START</div><h3>Connect an application</h3></div></div><div className="endpoint-example-body"><div><label>BASE URL</label><code>{baseUrl}</code></div><div><label>MODEL</label><code>Use any ID from GET /v1/models or a Combo alias</code></div><div><label>AUTHENTICATION</label><code>Authorization: Bearer YOUR_API_KEY</code></div></div></section>
 </div>;
}


function ModelCatalog({models,providers,onRefresh}:{models:Model[];providers:Provider[];onRefresh:()=>void}) {
 const [query,setQuery]=useState("");
 const [owner,setOwner]=useState("all");
 const filtered=models.filter(model=>{
  const text=(model.id+" "+(model.owned_by||"")).toLowerCase();
  return text.includes(query.trim().toLowerCase())&&(owner==="all"||model.owned_by===owner);
 }).sort((a,b)=>a.id.localeCompare(b.id));
 return <section className="panel">
  <div className="panel-toolbar">
   <div><b>Available models</b><span>{filtered.length} of {models.length} models exposed by the gateway</span></div>
   <button className="secondary" onClick={onRefresh}><Icon name="refresh" size={14}/> Refresh catalog</button>
  </div>
  <div className="model-filters">
   <input aria-label="Search models" value={query} onChange={e=>setQuery(e.target.value)} placeholder="Search model ID or provider…"/>
   <select aria-label="Filter by provider" value={owner} onChange={e=>setOwner(e.target.value)}>
    <option value="all">All providers</option>
    {providers.map(provider=><option key={provider.id} value={provider.id}>{provider.id}</option>)}
   </select>
  </div>
  <div className="table-scroll"><table><thead><tr><th>MODEL ID</th><th>PROVIDER</th><th>CLIENT MODEL NAME</th></tr></thead>
   <tbody>{filtered.map(model=><tr key={model.id}><td><code className="model-code">{model.id}</code></td><td>{model.owned_by||"—"}</td><td><code>{model.id}</code></td></tr>)}</tbody>
  </table>{filtered.length===0&&<div className="empty">{models.length===0?"No models are currently exposed. Test a provider and refresh the catalog.":"No models match this filter."}</div>}</div>
 </section>;
}
function Providers({providers,models,onRefresh,onAdd,onEdit,onTest,onDelete}:{providers:Provider[];models:Model[];onRefresh:()=>void;onAdd:()=>void;onEdit:(p:Provider)=>void;onTest:(id:string)=>void;onDelete:(id:string)=>void}){return <><div className="provider-cards">{providers.map((p,i)=>{const providerModels=models.filter(m=>m.owned_by===p.id);return <article className="provider-large" key={p.id}><div className="provider-card-head"><div className={"provider-logo big tone-"+(i%4)}>{p.id.slice(0,2).toUpperCase()}</div><span className={cls("tag",p.has_api_key?"green":"amber")}>{p.has_api_key?"CONNECTED":"NEEDS CREDENTIAL"}</span></div><h3>{p.id}</h3><p className="provider-url">{p.base_url}</p><div className="provider-stats"><div><small>MODELS</small><b>{providerModels.length|| (p.models?.includes("*")?"Auto":"—")}</b></div><div><small>TIMEOUT</small><b>{p.timeout_seconds}s</b></div><div><small>KEYS</small><b>{p.key_count||0}</b></div></div><div className="provider-key-line"><Icon name="key" size={14}/><span>{p.api_key_preview||"No credential configured"}{p.key_count?` · ${p.key_count} rotating keys`:""}</span></div>{providerModels.length>0&&<details className="provider-models"><summary>Available models <span>{providerModels.length}</span></summary><div>{providerModels.slice(0,12).map(m=><code key={m.id}>{m.id}</code>)}{providerModels.length>12&&<small>+ {providerModels.length-12} more</small>}</div></details>}<div className="provider-actions"><button className="secondary" onClick={()=>onTest(p.id)}><Icon name="zap" size={14}/> Test</button><button className="secondary" onClick={()=>onEdit(p)}>Configure</button><button className="icon-button" title="Delete provider" onClick={()=>onDelete(p.id)}><Icon name="trash" size={15}/></button></div></article>})}</div><button className="add-provider-card" onClick={onAdd}><span className="add-circle"><Icon name="plus" size={20}/></span><b>Add provider</b><span>Connect another OpenAI-compatible endpoint</span></button><details className="inline-tool-details"><summary>Browse searchable model catalog <span>{models.length} models</span></summary><ModelCatalog models={models} providers={providers} onRefresh={onRefresh}/></details></>}
function Routing({routes,providers,alias,setAlias,routeProvider,setRouteProvider,upstreamModel,setUpstreamModel,fallbackModels,setFallbackModels,onSave,onRemove}:{routes:Record<string,Route>;providers:Provider[];alias:string;setAlias:(v:string)=>void;routeProvider:string;setRouteProvider:(v:string)=>void;upstreamModel:string;setUpstreamModel:(v:string)=>void;fallbackModels:string;setFallbackModels:(v:string)=>void;onSave:(e:FormEvent)=>void;onRemove:(n:string)=>void}){return <div className="routing-layout"><section className="panel route-create"><div className="panel-heading"><div><div className="eyebrow">ROUTING COMBO</div><h3>Create a combo</h3></div><div className="icon-tile violet"><Icon name="route"/></div></div><p className="muted-copy">Expose a stable model name to your clients and map it to the actual model and provider upstream.</p><form onSubmit={onSave}><label>COMBO NAME</label><input required value={alias} onChange={e=>setAlias(e.target.value)} placeholder="e.g. fantom-fast"/><label>UPSTREAM PROVIDER</label><select required value={routeProvider} onChange={e=>setRouteProvider(e.target.value)}>{providers.map(p=><option key={p.id} value={p.id}>{p.id}</option>)}</select><label>UPSTREAM MODEL ID</label><input required value={upstreamModel} onChange={e=>setUpstreamModel(e.target.value)} placeholder="e.g. meta/llama-3.3-70b-instruct"/><details className="combo-advanced"><summary>Add fallback models (optional)</summary><label>FALLBACK MODEL ALIASES</label><input value={fallbackModels} onChange={e=>setFallbackModels(e.target.value)} placeholder="backup-fast, backup-stable"/><small>Try these public model IDs or aliases in order when the primary route hits an auth, rate-limit or temporary upstream error.</small></details><button className="primary wide" disabled={!providers.length}><Icon name="plus" size={15}/> Create combo</button></form></section><section className="panel"><div className="panel-heading"><div><div className="eyebrow">ACTIVE COMBOS</div><h3>Your combos</h3></div><span className="tag">{Object.keys(routes).length} ROUTES</span></div><div className="route-list">{Object.entries(routes).map(([name,route])=><div className="route-row" key={name}><div className="route-source"><code>{name}</code><small>PUBLIC MODEL</small></div><div className="route-arrow"><Icon name="route" size={17}/></div><div className="route-target"><b>{route.upstream_model}</b><small>{route.provider}</small>{route.fallback_models&&route.fallback_models.length>0&&<small className="fallback-hint">fallback: {route.fallback_models.join(" → ")}</small>}</div><button className="icon-button" onClick={()=>onRemove(name)} title="Remove route"><Icon name="trash" size={14}/></button></div>)}{Object.keys(routes).length===0&&<div className="empty">No combos yet. Create one to give your client a stable model name with automatic fallbacks.</div>}</div></section></div>}
function UsagePage({usage,requests}:{usage:Usage;requests:Log[]}){const max=Math.max(1,...Object.values(usage.by_model||{}));return <><div className="metrics-grid three"><Metric label="TOTAL REQUESTS" value={formatNumber(usage.total_requests)} detail="Requests recorded" glyph="chart" tone="violet"/><Metric label="SUCCESS RATE" value={usage.total_requests?((usage.successful_requests/usage.total_requests)*100).toFixed(1)+"%":"—"} detail={formatNumber(usage.successful_requests)+" successful"} glyph="check" tone="green"/><Metric label="FAILED REQUESTS" value={formatNumber(usage.failed_requests)} detail="Non-2xx responses" glyph="alert" tone="amber"/></div><div className="metrics-grid three usage-token-metrics"><Metric label="PROMPT TOKENS" value={formatNumber(usage.total_prompt_tokens||0)} detail="Reported by providers" glyph="terminal" tone="blue"/><Metric label="COMPLETION TOKENS" value={formatNumber(usage.total_completion_tokens||0)} detail="Reported by providers" glyph="terminal" tone="violet"/><Metric label="TOTAL TOKENS" value={formatNumber(usage.total_tokens||0)} detail="No estimates included" glyph="chart" tone="green"/></div><div className="content-grid"><section className="panel"><div className="panel-heading"><div><div className="eyebrow">MODEL DISTRIBUTION</div><h3>Requests by model</h3></div></div>{Object.entries(usage.by_model||{}).sort((a,b)=>b[1]-a[1]).slice(0,12).map(([name,count])=><div className="traffic-item" key={name}><div><span className="mono">{name}</span><b>{count}</b></div><div className="bar-track"><i className="bar-fill tone-1" style={{width:(count/max*100)+"%"}}/></div></div>)}{Object.keys(usage.by_model||{}).length===0&&<div className="empty">Model metrics will appear after traffic begins.</div>}</section><section className="panel"><div className="panel-heading"><div><div className="eyebrow">PERFORMANCE</div><h3>Gateway latency</h3></div></div><div className="latency-number">{usage.avg_latency_ms?formatNumber(usage.avg_latency_ms):"—"}<small>ms average</small></div><p className="muted-copy">Average time from request arrival until the upstream response stream completes. This is gateway-observed latency, not provider-reported inference time.</p><div className="info-row"><span>Measured requests</span><b>{formatNumber(usage.total_requests)}</b></div><div className="info-row"><span>Streaming requests</span><b>{requests.filter(r=>r.streaming).length}</b></div></section></div><section className="panel recent-panel"><div className="panel-heading"><div><div className="eyebrow">REQUEST DETAIL</div><h3>Latest activity</h3></div></div><RequestTable rows={requests.slice(0,20)}/></section><details className="inline-tool-details request-explorer"><summary>Open full request explorer <span>{requests.length} retained requests</span></summary><LogsPage requests={requests}/></details></>}

function LogsPage({requests}:{requests:Log[]}) {
 const [query,setQuery]=useState("");
 const [status,setStatus]=useState("all");
 const filtered=requests.filter(row=>{
  const q=query.trim().toLowerCase();
  const matches=!q||[row.id,row.model,row.provider].some(value=>(value||"").toLowerCase().includes(q));
  const statusMatches=status==="all"||(status==="success"?row.status>=200&&row.status<400:row.status<200||row.status>=400);
  return matches&&statusMatches;
 });
 return <section className="panel">
  <div className="panel-toolbar">
   <div><b>Gateway request history</b><span>{filtered.length} of {requests.length} recent requests · provider-reported tokens where available</span></div>
   <span className="tag">RECENT</span>
  </div>
  <div className="model-filters">
   <input aria-label="Search request logs" value={query} onChange={e=>setQuery(e.target.value)} placeholder="Search request ID, model or provider…"/>
   <select aria-label="Filter request status" value={status} onChange={e=>setStatus(e.target.value)}>
    <option value="all">All statuses</option>
    <option value="success">Successful</option>
    <option value="failed">Failed</option>
   </select>
  </div>
  <RequestTable rows={filtered}/>
  {filtered.length===0&&<div className="empty">{requests.length===0?"No requests have passed through this gateway yet.":"No requests match these filters."}</div>}
 </section>;
}
function RequestTable({rows}:{rows:Log[]}){return <div className="table-scroll"><table><thead><tr><th>REQUEST</th><th>MODEL</th><th>PROVIDER</th><th>STATUS</th><th>LATENCY</th><th>TOKENS</th><th>RETRIES</th><th>WHEN</th></tr></thead><tbody>{rows.map(r=><tr key={r.id+"-"+r.at}><td><code className="request-id">{(r.id||"—").slice(0,12)}</code></td><td><code className="model-code">{r.model||"—"}</code>{r.streaming&&<span className="stream-tag">SSE</span>}</td><td>{r.provider||"—"}</td><td><span className={cls("status-code",r.status>=200&&r.status<400?"good":"bad")}>{r.status||"—"}</span></td><td>{r.latency_ms} ms</td><td>{r.total_tokens?formatNumber(r.total_tokens):"—"}</td><td><span title={(r.upstream_attempts||0)+" upstream attempts"}>{r.credential_retries||0} key · {r.fallback_attempts||0} fallback</span></td><td>{ago(r.at)}</td></tr>)}</tbody></table></div>}
