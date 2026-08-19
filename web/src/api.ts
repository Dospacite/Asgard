import { useQuery } from '@tanstack/react-query'
import type { System } from './types'
const API = '/api/v1'

export class APIError extends Error { constructor(public status:number, public code:string, message:string, public details?:unknown){super(message)} }

function cookie(name:string){ return document.cookie.split('; ').find(v=>v.startsWith(`${name}=`))?.split('=').slice(1).join('=') ?? '' }

async function raw(path:string, init:RequestInit={}, retry=true):Promise<Response>{
  const headers = new Headers(init.headers)
  if (init.body && !(init.body instanceof FormData) && !headers.has('Content-Type')) headers.set('Content-Type','application/json')
  if (init.method && !['GET','HEAD'].includes(init.method.toUpperCase())) headers.set('X-CSRF-Token',decodeURIComponent(cookie('asgard_csrf')))
  const response = await fetch(path,{...init,headers,credentials:'include'})
  if(response.status===401 && retry && !path.endsWith('/auth/login') && !path.endsWith('/auth/refresh')){
    const refresh = await fetch(`${API}/auth/refresh`,{method:'POST',credentials:'include'})
    if(refresh.ok) return raw(path,init,false)
  }
  return response
}

export async function request<T>(path:string,init:RequestInit={}):Promise<T>{
  const endpoint=path===API||path.startsWith(`${API}/`)?path:`${API}/${path.replace(/^\/+/, '')}`
  const response=await raw(endpoint,init)
  if(!response.ok){let value:any={};try{value=await response.json()}catch{value={}};throw new APIError(response.status,value?.error?.code??value?.error??'request_failed',value?.error?.message??value?.error_description??`Request failed (${response.status})`,value?.error?.details)}
  if(response.status===204) return undefined as T
  return response.json() as Promise<T>
}

export const api={
  get:<T>(path:string)=>request<T>(path),
  post:<T>(path:string,body?:unknown,headers?:HeadersInit)=>request<T>(path,{method:'POST',body:body instanceof FormData?body:body===undefined?undefined:JSON.stringify(body),headers}),
  patch:<T>(path:string,body:unknown,headers?:HeadersInit)=>request<T>(path,{method:'PATCH',body:JSON.stringify(body),headers}),
  put:<T>(path:string,body:unknown,headers?:HeadersInit)=>request<T>(path,{method:'PUT',body:JSON.stringify(body),headers}),
  del:<T>(path:string,body?:unknown)=>request<T>(path,{method:'DELETE',body:body===undefined?undefined:JSON.stringify(body)}),
}

// useDomain returns the wildcard domain this control plane is configured with.
// Hostnames are an operator setting, not a constant: a self-hosted install runs
// under its own zone, so nothing in the UI may assume a particular one. React
// Query dedupes the shared 'system' key, so every caller costs one request.
export function useDomain(){
  const query=useQuery({queryKey:['system'],queryFn:()=>api.get<System>('/system'),staleTime:5*60_000})
  return query.data?.domain??''
}

// projectHostname is the name a project is reachable at: the hostname its
// public service actually claims, or the default derived from the slug when it
// has none yet.
export function projectHostname(project:{slug:string;services?:{public:boolean;hostname:string}[]},domain:string){
  const claimed=project.services?.find(service=>service.public&&service.hostname)?.hostname
  if(claimed)return claimed
  return domain?`${project.slug}.${domain}`:project.slug
}
