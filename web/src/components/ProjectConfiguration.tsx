import { FormEvent, KeyboardEvent, useEffect, useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { AlertCircle, Braces, CheckCircle2, Code2, FileCode2, FileKey2, Plus, RotateCcw, Save } from 'lucide-react'
import { APIError, api } from '../api'
import type { Project, ProjectSourceFile, ProjectSourceWorkspace, Service, SourceValidation, ValidationIssue } from '../types'
import { Button, ErrorState, Skeleton } from './ui'

type Mode = 'files'|'environment'

export default function ProjectConfiguration({project}:{project:Project}){
  const [mode,setMode]=useState<Mode>('files')
  const source=useQuery({queryKey:['project-source',project.id],queryFn:()=>api.get<ProjectSourceWorkspace>(`/projects/${project.id}/source-files`),staleTime:30_000})
  const chooseMode=(event:KeyboardEvent<HTMLButtonElement>)=>{
    if(event.key!=='ArrowLeft'&&event.key!=='ArrowRight')return
    event.preventDefault()
    const next=mode==='files'?'environment':'files'
    setMode(next)
    requestAnimationFrame(()=>document.getElementById(`configuration-${next}-tab`)?.focus())
  }
  return <div className="configuration-page">
    <div className="configuration-switcher" role="tablist" aria-label="Configuration workspace">
      <button id="configuration-files-tab" role="tab" aria-controls="configuration-panel" aria-selected={mode==='files'} tabIndex={mode==='files'?0:-1} className={mode==='files'?'active':''} onKeyDown={chooseMode} onClick={()=>setMode('files')}><Code2 aria-hidden/><span><strong>Source files</strong><small>Compose, Dockerfiles, and .env</small></span></button>
      <button id="configuration-environment-tab" role="tab" aria-controls="configuration-panel" aria-selected={mode==='environment'} tabIndex={mode==='environment'?0:-1} className={mode==='environment'?'active':''} onKeyDown={chooseMode} onClick={()=>setMode('environment')}><Braces aria-hidden/><span><strong>Environment</strong><small>Choose variables per service</small></span></button>
    </div>
    <div id="configuration-panel" role="tabpanel" aria-labelledby={`configuration-${mode}-tab`}>
      {source.isPending?<Skeleton height={540}/>:source.isError?<ErrorState error={source.error}/>:mode==='files'?<SourceEditor project={project} workspace={source.data}/>:<EnvironmentEditor project={project} workspace={source.data}/>} 
    </div>
  </div>
}

function SourceEditor({project,workspace}:{project:Project;workspace:ProjectSourceWorkspace}){
  const qc=useQueryClient()
  const [activePath,setActivePath]=useState(workspace.files[0]?.path??'')
  const [drafts,setDrafts]=useState<Record<string,string>>({})
  const [savedPath,setSavedPath]=useState('')
  useEffect(()=>{if(!workspace.files.some(file=>file.path===activePath))setActivePath(workspace.files[0]?.path??'')},[workspace.files,activePath])
  const active=workspace.files.find(file=>file.path===activePath)??workspace.files[0]
  const content=active?(drafts[active.path]??active.content):''
  const dirty=!!active&&content!==active.content
  const mutation=useMutation({
    mutationFn:(file:ProjectSourceFile)=>api.patch<ProjectSourceWorkspace>(`/projects/${project.id}/source-files`,{path:file.path,content:drafts[file.path]??file.content,revision:file.revision}),
    onSuccess:(next,file)=>{
      qc.setQueryData(['project-source',project.id],next)
      qc.invalidateQueries({queryKey:['project',project.id]})
      setDrafts(current=>{const copy={...current};delete copy[file.path];return copy})
      setSavedPath(file.path)
    },
  })
  if(!active)return <section className="panel source-empty"><h2>No editable source files</h2><p>Import a Compose project to create its source workspace.</p></section>
  const localIssues=active.kind==='dotenv'?workspace.dotenvErrors:active.validation?.errors??[]
  const serverValidation=validationFromError(mutation.error)
  const issues=serverValidation?.errors??localIssues
  return <section className="source-workspace" aria-label="Project source editor">
    <nav className="source-file-list" aria-label="Editable source files">
      <header><p className="eyebrow">FILES</p><h2>Project source</h2><p>Saved files become inputs to the next deployment.</p></header>
      {workspace.files.map(file=>{
        const fileDirty=(drafts[file.path]??file.content)!==file.content
        const invalid=file.kind==='compose'&&file.validation&&!file.validation.valid||file.kind==='dotenv'&&workspace.dotenvErrors.length>0
        const Icon=file.kind==='compose'?FileCode2:file.kind==='dotenv'?FileKey2:Code2
        return <button type="button" key={file.path} className={file.path===active.path?'active':''} aria-current={file.path===active.path?'true':undefined} onClick={()=>{mutation.reset();setActivePath(file.path);setSavedPath('')}}>
          <span className="source-file-list__icon"><Icon aria-hidden/></span><span><strong>{file.label}</strong><small>{file.path}</small></span><span className={`source-file-list__state ${invalid?'source-file-list__state--error':''}`}>{fileDirty?'Unsaved':invalid?'Invalid':file.exists?'Saved':'New'}</span>
        </button>
      })}
    </nav>
    <div className="source-editor-panel">
      <header className="source-editor-header"><div><p className="eyebrow">{active.kind.toUpperCase()}</p><h2>{active.path}</h2><p>{sourceHelp(active.kind)}</p></div><span className={dirty?'editor-state editor-state--dirty':'editor-state'}>{dirty?'Unsaved changes':active.exists?'Up to date':'Not created'}</span></header>
      <div className="source-editor-body">
        <label className="sr-only" htmlFor="source-content">{active.label} content</label>
        <textarea id="source-content" className="source-code" spellCheck={false} value={content} onChange={event=>{mutation.reset();setSavedPath('');setDrafts(current=>({...current,[active.path]:event.target.value}))}} aria-label={`${active.label} content`} aria-describedby="source-editor-help"/>
        <p id="source-editor-help" className="source-editor-help">{active.kind==='compose'?'Compose is validated against Asgard’s safe contract. Existing runtime overrides are preserved.':active.kind==='dotenv'?'.env values become candidates below; only variables selected for a service are deployed.':'Dockerfile changes are used the next time this project builds.'}</p>
        {issues.length>0?<IssueList title={serverValidation?'Couldn’t save this file':'Current validation issues'} issues={issues}/>:null}
        {mutation.error&&!serverValidation?<p className="form-error" role="alert">{mutation.error.message}</p>:null}
        {savedPath===active.path&&mutation.isSuccess?<div className="inline-success" role="status"><CheckCircle2 aria-hidden/>Saved {active.label}. Deploy when you are ready to apply it.</div>:null}
      </div>
      <footer className="source-editor-actions"><small>{dirty?'Review and save this file before deploying.':'No unsaved changes in this file.'}</small><div><Button type="button" variant="secondary" disabled={!dirty||mutation.isPending} onClick={()=>{mutation.reset();setDrafts(current=>{const copy={...current};delete copy[active.path];return copy});setSavedPath('')}}><RotateCcw aria-hidden/>Reset</Button><Button type="button" disabled={!dirty} busy={mutation.isPending} onClick={()=>mutation.mutate(active)}><Save aria-hidden/>Save file</Button></div></footer>
    </div>
  </section>
}

function EnvironmentEditor({project,workspace}:{project:Project;workspace:ProjectSourceWorkspace}){
  const qc=useQueryClient()
  const [serviceId,setServiceId]=useState(project.services[0]?.id??'')
  const [drafts,setDrafts]=useState<Record<string,Record<string,string>>>({})
  const [name,setName]=useState('')
  const [value,setValue]=useState('')
  const [addError,setAddError]=useState('')
  useEffect(()=>{if(!project.services.some(service=>service.id===serviceId))setServiceId(project.services[0]?.id??'')},[project.services,serviceId])
  const service=project.services.find(item=>item.id===serviceId)??project.services[0]
  const environment=service?(drafts[service.id]??service.environment):{}
  const keys=useMemo(()=>Array.from(new Set([...Object.keys(workspace.dotenv),...Object.keys(environment)])).sort(),[workspace.dotenv,environment])
  const dirty=!!service&&!sameEnvironment(environment,service.environment)
  const mutation=useMutation({
    mutationFn:({service,environment}:{service:Service;environment:Record<string,string>})=>api.patch<Service>(`/services/${service.id}`,servicePayload(service,environment),{'If-Match':`"${service.configRevision}"`}),
    onSuccess:(updated)=>{
      setDrafts(current=>{const copy={...current};delete copy[updated.id];return copy})
      qc.setQueryData(['service',updated.id],updated)
      qc.setQueryData<Project>(['project',project.id],current=>current?{...current,services:current.services.map(item=>item.id===updated.id?updated:item)}:current)
      qc.invalidateQueries({queryKey:['project',project.id]})
    },
  })
  if(!service)return <section className="panel source-empty"><h2>No services available</h2><p>Add a service in Compose before choosing environment variables.</p></section>
  const setEnvironment=(next:Record<string,string>)=>{mutation.reset();setDrafts(current=>({...current,[service.id]:next}))}
  const toggle=(key:string,checked:boolean)=>{
    const next={...environment}
    if(checked)next[key]=workspace.dotenv[key]??''
    else delete next[key]
    setEnvironment(next)
  }
  const add=(event:FormEvent)=>{
    event.preventDefault()
    const key=name.trim()
    if(!/^[A-Za-z_][A-Za-z0-9_]*$/.test(key)){setAddError('Use letters, numbers, and underscores; start with a letter or underscore.');return}
    setEnvironment({...environment,[key]:value})
    setName('');setValue('');setAddError('')
  }
  return <div className="environment-workspace">
    <section className="panel environment-panel">
      <header className="source-editor-header"><div><p className="eyebrow">RUNTIME ENVIRONMENT</p><h2>Choose variables</h2><p>Select values from .env or add an explicit variable for this service.</p></div><label className="service-picker">Service<select aria-label="Service" value={service.id} onChange={event=>{mutation.reset();setServiceId(event.target.value)}}>{project.services.map(item=><option value={item.id} key={item.id}>{item.name}</option>)}</select></label></header>
      {workspace.dotenvErrors.length>0?<IssueList title="Fix .env before using its values" issues={workspace.dotenvErrors}/>:null}
      <div className="environment-list" aria-label={`Environment variables for ${service.name}`}>
        {keys.length===0?<div className="environment-empty"><Braces aria-hidden/><strong>No variables yet</strong><p>Add values in .env or create an explicit variable below.</p></div>:keys.map(key=>{
          const selected=Object.prototype.hasOwnProperty.call(environment,key)
          const fromDotEnv=Object.prototype.hasOwnProperty.call(workspace.dotenv,key)
          const overridden=selected&&fromDotEnv&&environment[key]!==workspace.dotenv[key]
          return <div className="environment-row" key={key}>
            <input id={`env-${service.id}-${key}`} type="checkbox" aria-label={`Select ${key} for ${service.name}`} checked={selected} onChange={event=>toggle(key,event.target.checked)}/>
            <label htmlFor={`env-${service.id}-${key}`}><code>{key}</code><small>{fromDotEnv?(overridden?'Custom override':'.env candidate'):'Explicit variable'}</small></label>
            <span className="variable-source">{fromDotEnv?'.env':'custom'}</span>
            <label className="sr-only" htmlFor={`env-value-${service.id}-${key}`}>{key} value</label>
            <input id={`env-value-${service.id}-${key}`} aria-label={`${key} value`} autoComplete="off" value={selected?environment[key]:workspace.dotenv[key]??''} disabled={!selected} onChange={event=>setEnvironment({...environment,[key]:event.target.value})}/>
            {selected&&fromDotEnv&&overridden?<button type="button" className="use-source-value" onClick={()=>setEnvironment({...environment,[key]:workspace.dotenv[key]})}>Use .env value</button>:<span/>}
          </div>
        })}
      </div>
      <form className="environment-add" onSubmit={add}>
        <div><label htmlFor="environment-name">Variable name</label><input id="environment-name" aria-label="Variable name" autoComplete="off" placeholder="API_URL" value={name} onChange={event=>setName(event.target.value)}/></div>
        <div><label htmlFor="environment-value">Value</label><input id="environment-value" aria-label="Value" autoComplete="off" placeholder="https://…" value={value} onChange={event=>setValue(event.target.value)}/></div>
        <Button type="submit" variant="secondary"><Plus aria-hidden/>Add variable</Button>
        {addError?<p className="form-error" role="alert">{addError}</p>:null}
      </form>
      {mutation.error?<p className="form-error environment-save-error" role="alert">{mutation.error.message}</p>:null}
      {mutation.isSuccess&&!dirty?<div className="inline-success environment-save-success" role="status"><CheckCircle2 aria-hidden/>Environment saved for {service.name}.</div>:null}
      <footer className="source-editor-actions"><small>{Object.keys(environment).length} selected · changes apply on the next deployment.</small><div><Button type="button" variant="secondary" disabled={!dirty||mutation.isPending} onClick={()=>{mutation.reset();setDrafts(current=>{const copy={...current};delete copy[service.id];return copy})}}><RotateCcw aria-hidden/>Reset</Button><Button type="button" disabled={!dirty} busy={mutation.isPending} onClick={()=>mutation.mutate({service,environment})}><Save aria-hidden/>Save variables</Button></div></footer>
    </section>
  </div>
}

function sourceHelp(kind:ProjectSourceFile['kind']){
  if(kind==='compose')return 'Service definitions and build inputs'
  if(kind==='dotenv')return 'Reusable environment-value candidates'
  return 'Container build instructions'
}

function IssueList({title,issues}:{title:string;issues:ValidationIssue[]}){return <div className="source-issues" role="alert"><AlertCircle aria-hidden/><div><strong>{title}</strong><ul>{issues.map((issue,index)=><li key={`${issue.path}-${index}`}><code>{issue.path}</code> {issue.message}</li>)}</ul></div></div>}

function validationFromError(error:Error|null):SourceValidation|undefined{
  if(!(error instanceof APIError)||!error.details||typeof error.details!=='object')return undefined
  const validation=(error.details as {validation?:SourceValidation}).validation
  return validation&&Array.isArray(validation.errors)?validation:undefined
}

function sameEnvironment(left:Record<string,string>,right:Record<string,string>){
  const leftKeys=Object.keys(left).sort(),rightKeys=Object.keys(right).sort()
  return leftKeys.length===rightKeys.length&&leftKeys.every((key,index)=>key===rightKeys[index]&&left[key]===right[key])
}

function servicePayload(service:Service,environment:Record<string,string>){return {role:service.role,environment,public:service.public,port:service.port,hostname:service.hostname,healthPath:service.healthPath,hstsMode:service.hstsMode??'',cpuLimit:service.cpuLimit,memoryLimit:service.memoryLimit,pidsLimit:service.pidsLimit,restartPolicy:service.restartPolicy,configRevision:service.configRevision}}
