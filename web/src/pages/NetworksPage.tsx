import { FormEvent, useEffect, useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Box, FolderKanban, Globe2, Link2, Network, Plus, RefreshCw, Trash2, Unlink, Wifi, X } from 'lucide-react'
import { api } from '../api'
import { Button, EmptyState, ErrorState, PageHeader, Skeleton, Status } from '../components/ui'
import { Link } from '../router'
import type { ManagedNetwork, NetworkTopology, Project, TopologyConnection, TopologyNetwork } from '../types'

type Lens = 'network' | 'project' | 'endpoint'
type NetworkList = {items:ManagedNetwork[]}

const slugify=(value:string)=>value.toLowerCase().trim().replace(/[^a-z0-9]+/g,'-').replace(/^-|-$/g,'').slice(0,63).replace(/-$/,'')

export default function NetworksPage(){
  const queryClient=useQueryClient()
  const [lens,setLens]=useState<Lens>('network')
  const [showCreate,setShowCreate]=useState(false)
  const [selectedId,setSelectedId]=useState('')
  const [serviceId,setServiceId]=useState('')
  const [alias,setAlias]=useState('')
  const [deleteArmed,setDeleteArmed]=useState(false)
  const [draft,setDraft]=useState({name:'',slug:'',description:'',internal:false})
  const [slugEdited,setSlugEdited]=useState(false)

  const networks=useQuery({queryKey:['networks'],queryFn:()=>api.get<NetworkList>('/networks'),refetchInterval:10_000})
  const topology=useQuery({queryKey:['network-topology'],queryFn:()=>api.get<NetworkTopology>('/networks/topology'),refetchInterval:10_000})
  const projects=useQuery({queryKey:['projects'],queryFn:()=>api.get<{items:Project[]}>('/projects'),refetchInterval:15_000})
  const refresh=()=>Promise.all([
    queryClient.invalidateQueries({queryKey:['networks']}),
    queryClient.invalidateQueries({queryKey:['network-topology']}),
    queryClient.invalidateQueries({queryKey:['projects']}),
  ])

  const create=useMutation({
    mutationFn:()=>api.post<ManagedNetwork>('/networks',draft),
    onSuccess:async item=>{setDraft({name:'',slug:'',description:'',internal:false});setSlugEdited(false);setShowCreate(false);await refresh();setSelectedId(item.id)},
  })
  const attach=useMutation({
    mutationFn:()=>api.post<ManagedNetwork>(`/networks/${selectedId}/members`,{serviceId,alias}),
    onSuccess:async()=>{setAlias('');await refresh()},
  })
  const detach=useMutation({
    mutationFn:(memberServiceId:string)=>api.del<ManagedNetwork>(`/networks/${selectedId}/members/${memberServiceId}`),
    onSuccess:refresh,
  })
  const reconcile=useMutation({mutationFn:()=>api.post<ManagedNetwork>(`/networks/${selectedId}/reconcile`),onSuccess:refresh})
  const remove=useMutation({
    mutationFn:async()=>{const preview=await api.post<{token:string}>('/deletions/preview',{targetType:'network',targetId:selectedId});await api.post('/deletions/confirm',{token:preview.token})},
    onSuccess:async()=>{setDeleteArmed(false);setSelectedId('');await refresh()},
  })

  const items=networks.data?.items||[]
  const selected=items.find(item=>item.id===selectedId)||items[0]
  const allServices=useMemo(()=>(projects.data?.items||[]).flatMap(project=>project.services.map(service=>({service,project}))),[projects.data])
  const available=useMemo(()=>allServices.filter(({service})=>!selected?.members.some(member=>member.serviceId===service.id)),[allServices,selected])

  useEffect(()=>{if(!selectedId&&items[0])setSelectedId(items[0].id);if(selectedId&&items.length&&!items.some(item=>item.id===selectedId))setSelectedId(items[0].id)},[items,selectedId])
  useEffect(()=>{if(!available.some(({service})=>service.id===serviceId))setServiceId(available[0]?.service.id||'')},[available,serviceId])
  useEffect(()=>setDeleteArmed(false),[selected?.id])

  if(networks.isPending||topology.isPending||projects.isPending)return <div className="page"><PageHeader eyebrow="CONNECTIVITY" title="Networks" description="Loading live Docker topology and persisted service memberships."/><Skeleton height={460}/></div>
  if(networks.isError)return <div className="page"><ErrorState error={networks.error}/></div>
  if(topology.isError)return <div className="page"><ErrorState error={topology.error}/></div>
  if(projects.isError)return <div className="page"><ErrorState error={projects.error}/></div>

  const attached=items.reduce((total,item)=>total+item.members.length,0)
  const crossProject=items.filter(item=>new Set(item.members.map(member=>member.projectId)).size>1).length
  const issues=items.reduce((total,item)=>total+(item.status!=='active'?1:0)+item.members.filter(member=>member.dockerId&&!member.connected).length,0)
  const submitCreate=(event:FormEvent)=>{event.preventDefault();create.mutate()}
  const updateName=(name:string)=>setDraft(current=>({...current,name,slug:slugEdited?current.slug:slugify(name)}))

  return <div className="page networks-page">
    <PageHeader eyebrow="CONNECTIVITY" title="Networks" description="Connect selected services across project boundaries with stable private DNS, without opening another public route." actions={<Button onClick={()=>setShowCreate(value=>!value)}>{showCreate?<X aria-hidden/>:<Plus aria-hidden/>}{showCreate?'Close':'Create network'}</Button>}/>

    {showCreate?<form className="panel network-create" onSubmit={submitCreate}>
      <div className="section-heading"><div><p className="eyebrow">NEW SHARED BRIDGE</p><h2>Create a network</h2></div><Network aria-hidden/></div>
      <div className="network-create__fields">
        <label>Network name<input autoFocus required maxLength={100} value={draft.name} onChange={event=>updateName(event.target.value)} placeholder="Shared services"/></label>
        <label>DNS-safe slug<input required maxLength={63} pattern="[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?" value={draft.slug} onChange={event=>{setSlugEdited(true);setDraft(current=>({...current,slug:slugify(event.target.value)}))}} placeholder="shared-services"/><small>Docker network: asgard-shared-{draft.slug||'slug'}</small></label>
        <label className="span-2">Description<textarea maxLength={1000} value={draft.description} onChange={event=>setDraft(current=>({...current,description:event.target.value}))} placeholder="What should be allowed to communicate here?"/></label>
        <label className="check-field span-2"><input type="checkbox" checked={draft.internal} onChange={event=>setDraft(current=>({...current,internal:event.target.checked}))}/><span><strong>No external gateway on this network</strong><small>Members can communicate with each other; this attachment does not provide internet egress.</small></span></label>
      </div>
      {create.error?<p className="form-error" role="alert">{create.error.message}</p>:null}
      <div className="form-actions"><Button type="button" variant="secondary" onClick={()=>setShowCreate(false)}>Cancel</Button><Button busy={create.isPending}><Plus/>Create network</Button></div>
    </form>:null}

    <div className="metric-grid network-metrics">
      <div className="metric"><p>Shared networks</p><strong>{items.length}</strong><span>Explicit trust boundaries</span></div>
      <div className="metric"><p>Cross-project</p><strong>{crossProject}</strong><span>Networks spanning 2+ projects</span></div>
      <div className="metric"><p>Attached services</p><strong>{attached}</strong><span>Persisted memberships</span></div>
      <div className="metric"><p>Needs attention</p><strong>{issues}</strong><span>Missing networks or endpoints</span></div>
    </div>

    <section aria-labelledby="topology-heading">
      <div className="topology-heading"><div><p className="eyebrow">LIVE TOPOLOGY</p><h2 id="topology-heading">How workloads connect</h2><p>Every view describes the same live graph from a different angle.</p></div><div className="topology-legend" aria-label="Topology legend"><span><i className="kind-dot kind-dot--edge"/>Public edge</span><span><i className="kind-dot kind-dot--shared"/>Shared</span><span><i className="kind-dot kind-dot--project"/>Project private</span></div></div>
      <nav className="tabs topology-tabs" aria-label="Topology views">
        <button className={lens==='network'?'active':''} aria-pressed={lens==='network'} onClick={()=>setLens('network')}>Network map</button>
        <button className={lens==='project'?'active':''} aria-pressed={lens==='project'} onClick={()=>setLens('project')}>By project</button>
        <button className={lens==='endpoint'?'active':''} aria-pressed={lens==='endpoint'} onClick={()=>setLens('endpoint')}>Live endpoints</button>
      </nav>
      {lens==='network'?<NetworkMap topology={topology.data}/>:lens==='project'?<ProjectMap topology={topology.data}/>:<EndpointTable topology={topology.data}/>} 
    </section>

    <section className="network-admin" aria-labelledby="management-heading">
      <div className="topology-heading"><div><p className="eyebrow">MANAGEMENT</p><h2 id="management-heading">Shared application networks</h2><p>Membership changes are applied live and retained across deployments.</p></div></div>
      {items.length===0?<EmptyState icon={<Network/>} title="No shared networks" action={<Button onClick={()=>setShowCreate(true)}><Plus/>Create your first network</Button>}>Projects remain isolated by default. Create a shared network only where a private dependency crosses project boundaries.</EmptyState>:<div className="network-admin__grid">
        <div className="network-directory" aria-label="Shared networks">{items.map(item=><button key={item.id} className={selected?.id===item.id?'active':''} aria-current={selected?.id===item.id?'true':undefined} onClick={()=>setSelectedId(item.id)}><span className="network-directory__icon"><Network aria-hidden/></span><span><strong>{item.name}</strong><small>{item.members.length} service{item.members.length===1?'':'s'} · {item.internal?'internal':'egress capable'}</small></span><Status value={item.status}/></button>)}</div>
        {selected?<div className="panel network-detail">
          <header className="network-detail__header"><div><div className="network-title"><span className="network-directory__icon"><Wifi aria-hidden/></span><div><h3>{selected.name}</h3><code>{selected.dockerName}</code></div></div><p>{selected.description||'No description has been added.'}</p></div><div className="button-group"><Button variant="secondary" busy={reconcile.isPending} onClick={()=>reconcile.mutate()}><RefreshCw/>Reconcile</Button></div></header>
          <dl className="network-facts"><div><dt>Driver</dt><dd>{selected.driver}</dd></div><div><dt>Gateway policy</dt><dd>{selected.internal?'Internal only':'Outbound available'}</dd></div><div><dt>Subnet</dt><dd><code>{selected.runtime?.subnets.join(', ')||'Pending'}</code></dd></div><div><dt>Runtime</dt><dd><Status value={selected.status}/></dd></div></dl>
          <div className="network-members"><div className="network-subheading"><div><p className="eyebrow">MEMBERS</p><h3>Service endpoints</h3></div><span>{selected.members.length}</span></div>{selected.members.length===0?<p className="network-empty">No services are attached yet.</p>:selected.members.map(member=><div className="network-member" key={member.serviceId}><span className="service-node"><Box aria-hidden/></span><span><Link to={`/services/${member.serviceId}`}>{member.projectName} / {member.serviceName}</Link><code>{member.alias}</code></span><span className="network-member__address">{member.ipv4Address||'No live address'}</span><Status value={member.connected?'connected':member.dockerId?'disconnected':'pending'}/><Button variant="ghost" busy={detach.isPending&&detach.variables===member.serviceId} onClick={()=>detach.mutate(member.serviceId)}><Unlink/>Disconnect</Button></div>)}</div>
          <form className="network-attach" onSubmit={event=>{event.preventDefault();attach.mutate()}}><div className="network-subheading"><div><p className="eyebrow">ATTACH</p><h3>Add a service</h3></div><Link2 aria-hidden/></div><div className="network-attach__fields"><label>Project / service<select value={serviceId} disabled={!available.length} onChange={event=>setServiceId(event.target.value)}>{available.length?available.map(({service,project})=><option key={service.id} value={service.id}>{project.name} / {service.name}</option>):<option value="">Every service is attached</option>}</select></label><label>Private DNS alias<input value={alias} onChange={event=>setAlias(event.target.value)} placeholder={available[0]?`${available[0].project.slug}--${slugify(available.find(item=>item.service.id===serviceId)?.service.name||available[0].service.name)}`:'project--service'}/><small>Optional; defaults to project--service and is unique within this network.</small></label><Button disabled={!serviceId} busy={attach.isPending}><Link2/>Attach service</Button></div>{attach.error?<p className="form-error" role="alert">{attach.error.message}</p>:null}{detach.error?<p className="form-error" role="alert">{detach.error.message}</p>:null}{reconcile.error?<p className="form-error" role="alert">{reconcile.error.message}</p>:null}</form>
          <div className="network-danger"><div><strong>Delete network</strong><p>{selected.members.length?'Disconnect all services before this network can be deleted.':'The empty Docker bridge and its Asgard record will be removed.'}</p></div>{deleteArmed?<div className="network-danger__confirm"><span>Delete <strong>{selected.name}</strong>?</span><Button variant="secondary" onClick={()=>setDeleteArmed(false)}>Cancel</Button><Button variant="danger" busy={remove.isPending} onClick={()=>remove.mutate()}><Trash2/>Confirm delete</Button></div>:<Button variant="danger" disabled={selected.members.length>0} onClick={()=>setDeleteArmed(true)}><Trash2/>Delete network</Button>}</div>
          {remove.error?<p className="form-error network-delete-error" role="alert">{remove.error.message}</p>:null}
        </div>:null}
      </div>}
    </section>
  </div>
}

function NetworkMap({topology}:{topology:NetworkTopology}){
  const groups:[TopologyNetwork['kind'],string,string][]=[['edge','Public edge','Internet-facing routes terminate at Traefik.'],['shared','Shared application networks','Explicit bridges can span projects.'],['project','Project-private networks','Every project receives an isolated default bridge.']]
  return <div className="topology-map">{groups.map(([kind,title,description])=>{const nodes=topology.networks.filter(network=>network.kind===kind);if(!nodes.length)return null;return <section className={`topology-lane topology-lane--${kind}`} key={kind}><header><span className="topology-lane__icon">{kind==='edge'?<Globe2/>:kind==='shared'?<Network/>:<FolderKanban/>}</span><div><h3>{title}</h3><p>{description}</p></div></header><div className="topology-lane__nodes">{nodes.map(network=><NetworkNode key={network.id} network={network} topology={topology}/>)}</div></section>})}</div>
}

function NetworkNode({network,topology}:{network:TopologyNetwork;topology:NetworkTopology}){
  const connections=topology.connections.filter(connection=>connection.networkId===network.id)
  return <article className="topology-node"><div className="topology-node__network"><span className={`kind-badge kind-badge--${network.kind}`}>{network.kind==='project'?'private':network.kind}</span><h4>{network.name}</h4><code>{network.subnets.join(', ')||network.dockerName}</code><Status value={network.status}/></div><div className="topology-rail" aria-hidden/><div className="topology-node__services">{connections.length?connections.map(connection=>{const service=topology.services.find(item=>item.id===connection.serviceId);const project=topology.projects.find(item=>item.id===connection.projectId);return <Link className={`topology-service topology-service--${connection.status}`} to={`/services/${connection.serviceId}`} key={connection.id}><Box aria-hidden/><span><strong>{service?.name||'Unknown service'}</strong><small>{project?.name||'Unknown project'} · {connection.alias||'no alias'}</small></span><Status value={connection.status}/></Link>}):<span className="topology-node__empty">No connected services</span>}</div></article>
}

function ProjectMap({topology}:{topology:NetworkTopology}){
  if(!topology.projects.length)return <EmptyState icon={<FolderKanban/>} title="No projects">Import a project to see its private and shared network paths.</EmptyState>
  return <div className="project-topology">
    {topology.projects.map(project=>{
      const services=topology.services.filter(service=>service.projectId===project.id)
      return <article className="project-topology__card" key={project.id}>
        <header><span><FolderKanban aria-hidden/></span><div><Link to={`/projects/${project.id}`}>{project.name}</Link><small>{project.slug} · {services.length} service{services.length===1?'':'s'}</small></div><Status value={project.status}/></header>
        <div>{services.map(service=>{
          const paths=topology.connections.filter(connection=>connection.serviceId===service.id)
          return <div className="project-service" key={service.id}>
            <span className="service-node"><Box aria-hidden/></span>
            <span><Link to={`/services/${service.id}`}>{service.name}</Link><small>{service.state}</small></span>
            <div className="project-service__paths">{paths.map(connection=>{
              const targetNetwork=topology.networks.find(item=>item.id===connection.networkId)
              return <span className={`path-chip path-chip--${connection.kind}`} key={connection.id}><i/>{targetNetwork?.kind==='project'?'Private':targetNetwork?.name}<small>{connection.alias}</small></span>
            })}</div>
          </div>
        })}</div>
      </article>
    })}
  </div>
}

function EndpointTable({topology}:{topology:NetworkTopology}){
  const rows=topology.connections.map(connection=>({connection,service:topology.services.find(item=>item.id===connection.serviceId),project:topology.projects.find(item=>item.id===connection.projectId),network:topology.networks.find(item=>item.id===connection.networkId)}))
  return <section className="table-card endpoint-table"><div className="table-card__heading"><div><p className="eyebrow">DOCKER ENDPOINTS</p><h2>Live connection inventory</h2></div><span className="pill">{rows.length} paths</span></div>{rows.length===0?<p className="table-empty">No service endpoints are available.</p>:<table><thead><tr><th>Alias</th><th>Network</th><th>Service / project</th><th>State</th><th>Address</th></tr></thead><tbody>{rows.map(({connection,service,project,network})=><tr key={connection.id}><td><code>{connection.alias||'—'}</code></td><td><span className={`kind-badge kind-badge--${connection.kind}`}>{connection.kind==='project'?'private':connection.kind}</span><small className="block">{network?.name}</small></td><td><Link className="table-primary" to={`/services/${service?.id}`}><Box/>{service?.name||'Unknown'}</Link><small className="block">{project?.name}</small></td><td><Status value={connection.status}/></td><td><code>{connection.ipv4Address||'—'}</code></td></tr>)}</tbody></table>}</section>
}
