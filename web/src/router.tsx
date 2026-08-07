import { Children, createContext, isValidElement, MouseEvent, ReactElement, ReactNode, useContext, useEffect, useMemo, useState } from 'react'

type Location={pathname:string;search:string}
type NavigateOptions={replace?:boolean}
type RouterValue={location:Location;navigate:(to:string|number,options?:NavigateOptions)=>void;params:Record<string,string>;outlet?:ReactNode}
const RouterContext=createContext<RouterValue|null>(null)

function current():Location{return {pathname:window.location.pathname||'/',search:window.location.search}}
export function BrowserRouter({children}:{children:ReactNode}){const [location,setLocation]=useState(current);useEffect(()=>{const update=()=>setLocation(current());window.addEventListener('popstate',update);return()=>window.removeEventListener('popstate',update)},[]);const navigate=(to:string|number,options:NavigateOptions={})=>{if(typeof to==='number'){history.go(to);return}const target=new URL(to,window.location.href);if(target.origin!==window.location.origin){window.location.assign(target.href);return}(options.replace?history.replaceState:history.pushState).call(history,null,'',target.pathname+target.search+target.hash);setLocation(current());window.scrollTo({top:0,behavior:'instant'})};return <RouterContext.Provider value={{location,navigate,params:{}}}>{children}</RouterContext.Provider>}

type RouteProps={path?:string;index?:boolean;element?:ReactNode;children?:ReactNode}
export function Route(_:RouteProps){return null}

function parts(path:string){return path.replace(/^\/+|\/+$/g,'').split('/').filter(Boolean)}
function match(pattern:string|undefined,pathname:string,index?:boolean):{params:Record<string,string>;matched:boolean;score:number}{if(index)return {params:{},matched:pathname==='/'||pathname==='',score:1000};if(!pattern)return {params:{},matched:true,score:0};if(pattern==='*'||pattern==='/*')return {params:{},matched:true,score:-100};const wildcard=pattern.endsWith('/*');const expected=parts(wildcard?pattern.slice(0,-2):pattern);const actual=parts(pathname);if((!wildcard&&expected.length!==actual.length)||(wildcard&&actual.length<expected.length))return {params:{},matched:false,score:0};const params:Record<string,string>={};let score=expected.length*10;for(let i=0;i<expected.length;i++){const token=expected[i];if(token.startsWith(':')){params[token.slice(1)]=decodeURIComponent(actual[i]||'');score+=1}else if(token!==actual[i])return {params:{},matched:false,score:0};else score+=5}return {params,matched:true,score}}

function routeElements(children:ReactNode){return Children.toArray(children).filter(isValidElement) as ReactElement<RouteProps>[]}
function resolve(children:ReactNode,pathname:string):{node:ReactNode;params:Record<string,string>}|null{const candidates=routeElements(children).map(element=>({element,result:match(element.props.path,pathname,element.props.index)})).filter(item=>item.result.matched).sort((a,b)=>b.result.score-a.result.score);for(const {element,result} of candidates){if(element.props.children){const nested=resolve(element.props.children,pathname);if(nested)return {node:<OutletProvider outlet={nested.node} params={{...result.params,...nested.params}}>{element.props.element}</OutletProvider>,params:{...result.params,...nested.params}}}if(element.props.element!==undefined)return {node:element.props.element,params:result.params}}return null}
function OutletProvider({outlet,params,children}:{outlet:ReactNode;params:Record<string,string>;children:ReactNode}){const parent=useRouter();return <RouterContext.Provider value={{...parent,params,outlet}}>{children}</RouterContext.Provider>}
export function Routes({children}:{children:ReactNode}){const parent=useRouter();const resolved=resolve(children,parent.location.pathname);if(!resolved)return null;return <RouterContext.Provider value={{...parent,params:resolved.params}}>{resolved.node}</RouterContext.Provider>}
export function Outlet(){return <>{useRouter().outlet}</>}

export function useRouter(){const value=useContext(RouterContext);if(!value)throw new Error('Router context is missing');return value}
export function useLocation(){return useRouter().location}
export function useNavigate(){return useRouter().navigate}
export function useParams<T extends Record<string,string|undefined>=Record<string,string>>(){return useRouter().params as T}
export function useSearchParams():[URLSearchParams,(next:URLSearchParams|Record<string,string>)=>void]{const {location,navigate}=useRouter();const value=useMemo(()=>new URLSearchParams(location.search),[location.search]);const set=(next:URLSearchParams|Record<string,string>)=>{const params=next instanceof URLSearchParams?next:new URLSearchParams(next);navigate(location.pathname+(params.size?`?${params}`:''))};return [value,set]}
export function Navigate({to,replace=false}:{to:string;replace?:boolean}){const navigate=useNavigate();useEffect(()=>navigate(to,{replace}),[navigate,to,replace]);return null}

type LinkProps=Omit<React.AnchorHTMLAttributes<HTMLAnchorElement>,'href'>&{to:string}
export function Link({to,onClick,children,...props}:LinkProps){const navigate=useNavigate();const click=(event:MouseEvent<HTMLAnchorElement>)=>{onClick?.(event);if(event.defaultPrevented||event.button!==0||event.metaKey||event.ctrlKey||event.shiftKey||event.altKey)return;event.preventDefault();navigate(to)};return <a href={to} onClick={click} {...props}>{children}</a>}
export function NavLink({to,end=false,className,children,...props}:LinkProps&{end?:boolean}){const location=useLocation();const target=new URL(to,window.location.origin).pathname;const active=end?location.pathname===target:location.pathname===target||location.pathname.startsWith(target.endsWith('/')?target:`${target}/`);const classes=[typeof className==='string'?className:'',active?'active':''].filter(Boolean).join(' ');return <Link to={to} className={classes} {...props}>{children}</Link>}
