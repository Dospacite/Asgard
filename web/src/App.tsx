import { useQuery } from '@tanstack/react-query'
import { Navigate, Route, Routes, useLocation } from './router'
import { api } from './api'
import Shell from './components/Shell'
import { Skeleton } from './components/ui'
import AuditPage from './pages/AuditPage'
import DeploymentsPage from './pages/DeploymentsPage'
import ImportPage from './pages/ImportPage'
import LoginPage from './pages/LoginPage'
import NetworksPage from './pages/NetworksPage'
import NotFoundPage from './pages/NotFoundPage'
import OverviewPage from './pages/OverviewPage'
import ProjectPage from './pages/ProjectPage'
import ProjectsPage from './pages/ProjectsPage'
import ServicePage from './pages/ServicePage'
import SettingsPage from './pages/SettingsPage'
import StoragePage from './pages/StoragePage'
import UnmanagedPage from './pages/UnmanagedPage'

export type User={id:string;username:string;createdAt:string;lastLoginAt?:string}

function Authenticated(){const location=useLocation();const me=useQuery({queryKey:['me'],queryFn:()=>api.get<User>('/me'),retry:false});if(me.isPending)return <div className="boot"><div className="brand brand--boot"><span className="brand__rune">A</span><strong>Asgard</strong></div><Skeleton height={4}/></div>;if(me.isError)return <Navigate to={`/login?returnTo=${encodeURIComponent(location.pathname+location.search)}`} replace/>;return <Routes><Route element={<Shell/>}><Route index element={<OverviewPage/>}/><Route path="projects" element={<ProjectsPage/>}/><Route path="projects/new" element={<ImportPage/>}/><Route path="projects/:projectId" element={<ProjectPage/>}/><Route path="services/:serviceId" element={<ServicePage/>}/><Route path="deployments" element={<DeploymentsPage/>}/><Route path="networks" element={<NetworksPage/>}/><Route path="storage" element={<StoragePage/>}/><Route path="unmanaged" element={<UnmanagedPage/>}/><Route path="audit" element={<AuditPage/>}/><Route path="settings" element={<SettingsPage/>}/><Route path="*" element={<NotFoundPage/>}/></Route></Routes>}

export default function App(){return <Routes><Route path="/login" element={<LoginPage/>}/><Route path="/*" element={<Authenticated/>}/></Routes>}
