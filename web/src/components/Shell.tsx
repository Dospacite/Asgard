import { useState } from 'react'
import { NavLink, Outlet, useLocation } from '../router'
import { Activity, Boxes, CloudCog, DatabaseBackup, FolderKanban, Menu, Network, ScrollText, ServerCog, Settings, ShipWheel, X } from 'lucide-react'
import { IconButton } from './ui'

const links=[
  {to:'/',label:'Overview',icon:Activity,end:true},
  {to:'/projects',label:'Projects',icon:FolderKanban},
  {to:'/deployments',label:'Deployments',icon:ShipWheel},
  {to:'/networks',label:'Networks',icon:Network},
  {to:'/storage',label:'Storage',icon:DatabaseBackup},
  {to:'/unmanaged',label:'Unmanaged',icon:Boxes},
  {to:'/audit',label:'Audit',icon:ScrollText},
  {to:'/settings',label:'Settings',icon:Settings},
]

export default function Shell(){const [open,setOpen]=useState(false);const location=useLocation();return <div className="app-shell"><a className="skip-link" href="#main-content">Skip to content</a><aside className={`sidebar ${open?'sidebar--open':''}`} aria-label="Primary navigation"><div className="brand"><span className="brand__mark"><CloudCog aria-hidden/></span><span><strong>Asgard</strong><small>Control plane</small></span><IconButton label="Close navigation" onClick={()=>setOpen(false)}><X/></IconButton></div><nav>{links.map(({to,label,icon:Icon,end})=><NavLink key={to} to={to} end={end} onClick={()=>setOpen(false)}><Icon aria-hidden/><span>{label}</span></NavLink>)}</nav><div className="sidebar__footer"><ServerCog aria-hidden/><span><strong>Single VPS</strong><small>asgard.rousoftware.com</small></span></div></aside>{open?<button className="sidebar-scrim" aria-label="Close navigation" onClick={()=>setOpen(false)}/>:null}<div className="app-main"><header className="mobile-bar"><IconButton label="Open navigation" onClick={()=>setOpen(true)}><Menu/></IconButton><strong>Asgard</strong><span className="status-dot" aria-label="Control plane online"/></header><main id="main-content" key={location.pathname}><Outlet/></main></div></div>}
