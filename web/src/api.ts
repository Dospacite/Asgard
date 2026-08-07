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
  del:<T>(path:string,body?:unknown)=>request<T>(path,{method:'DELETE',body:body===undefined?undefined:JSON.stringify(body)}),
}
