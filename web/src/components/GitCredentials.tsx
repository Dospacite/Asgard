import { FormEvent, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { KeyRound, Plus, Trash2 } from 'lucide-react'
import { api } from '../api'
import { Button } from './ui'
import type { GitCredential } from '../types'

type Kind = 'token' | 'ssh'

export default function GitCredentials() {
  const qc = useQueryClient()
  const query = useQuery({ queryKey: ['git-credentials'], queryFn: () => api.get<{ items: GitCredential[] }>('/git-credentials') })
  const [open, setOpen] = useState(false)
  const [kind, setKind] = useState<Kind>('token')
  const [name, setName] = useState('')
  const [host, setHost] = useState('')
  const [username, setUsername] = useState('')
  const [secret, setSecret] = useState('')
  const reset = () => { setName(''); setHost(''); setUsername(''); setSecret(''); setOpen(false) }
  const create = useMutation({
    mutationFn: () => api.post<GitCredential>('/git-credentials', { name, kind, host, username, secret }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['git-credentials'] }); reset() },
  })
  const remove = useMutation({
    mutationFn: (id: string) => api.del<void>(`/git-credentials/${id}`),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['git-credentials'] }),
  })
  const submit = (event: FormEvent) => { event.preventDefault(); create.mutate() }
  const items = query.data?.items ?? []
  return (
    <section className="panel">
      <div className="section-heading">
        <div>
          <p className="eyebrow">SOURCE ACCESS</p>
          <h2>Git credentials</h2>
        </div>
        <KeyRound />
      </div>
      <p className="panel-hint">Secrets are encrypted with a host-local key and are never returned by the API. They are used only while cloning, and never written into a project's source.</p>
      {items.length === 0 ? <p className="empty-hint">No credentials stored. Public repositories import without one.</p> : (
        <ul className="credential-list">
          {items.map(item => (
            <li key={item.id}>
              <div>
                <strong>{item.name}</strong>
                <small>{item.kind === 'ssh' ? 'SSH deploy key' : `token · ${item.username}`}{item.host ? ` · ${item.host}` : ''}{item.hint ? ` · ${item.hint}` : ''}</small>
                <small>{item.lastUsedAt ? `Last used ${new Date(item.lastUsedAt).toLocaleString()}` : 'Never used'}</small>
              </div>
              <Button variant="secondary" busy={remove.isPending && remove.variables === item.id} onClick={() => remove.mutate(item.id)}><Trash2 aria-hidden />Delete</Button>
            </li>
          ))}
        </ul>
      )}
      {remove.error ? <p className="form-error" role="alert">{remove.error.message}</p> : null}
      {open ? (
        <form className="credential-form" onSubmit={submit}>
          <div className="form-grid">
            <label>Name<input required value={name} onChange={e => setName(e.target.value)} placeholder="github-deploy" /></label>
            <label>Kind<select value={kind} onChange={e => setKind(e.target.value as Kind)}><option value="token">Access token (HTTPS)</option><option value="ssh">SSH deploy key</option></select></label>
            <label>Host <span>(optional)</span><input value={host} onChange={e => setHost(e.target.value.toLowerCase())} placeholder="github.com" /></label>
            {kind === 'token' ? <label>Username <span>(optional)</span><input value={username} onChange={e => setUsername(e.target.value)} placeholder="x-access-token" /></label> : null}
            <label className="span-2">{kind === 'token' ? 'Token' : 'Private key (PEM)'}
              {kind === 'token'
                ? <input required type="password" value={secret} onChange={e => setSecret(e.target.value)} placeholder="ghp_…" autoComplete="off" />
                : <textarea required rows={6} value={secret} onChange={e => setSecret(e.target.value)} placeholder={'-----BEGIN OPENSSH PRIVATE KEY-----\n…\n-----END OPENSSH PRIVATE KEY-----'} />}
              <small>Write-only. Store a deploy key or a fine-grained token scoped to the repositories you intend to import.</small>
            </label>
          </div>
          {create.error ? <p className="form-error" role="alert">{create.error.message}</p> : null}
          <footer className="form-actions">
            <Button type="button" variant="secondary" onClick={reset}>Cancel</Button>
            <Button type="submit" busy={create.isPending}>Store credential</Button>
          </footer>
        </form>
      ) : <Button variant="secondary" onClick={() => setOpen(true)}><Plus aria-hidden />Add credential</Button>}
    </section>
  )
}
