import { FormEvent, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { KeyRound, Plus, RefreshCw, ShieldAlert, ShieldCheck, ShieldQuestion, Trash2 } from 'lucide-react'
import { api } from '../api'
import { Button } from './ui'
import type { GitCredential, VerifyResult } from '../types'

type Kind = 'token' | 'ssh'
type CredentialResponse = { credential: GitCredential; verification?: VerifyResult }

// A stored credential is only a guess until something has used it, and
// lastUsedAt is worse than nothing on its own: it goes green because the
// credential worked for some other project, while the repository it is attached
// to here rejects it. Verification state is the first thing each row reports.
function VerifyBadge({ item }: { item: GitCredential }) {
  const status = item.lastVerifyStatus
  if (status === 'ok') return <span className="verify verify--ok"><ShieldCheck aria-hidden />Verified{item.lastVerifiedAt ? ` ${new Date(item.lastVerifiedAt).toLocaleDateString()}` : ''}</span>
  if (status === 'failed') return <span className="verify verify--bad"><ShieldAlert aria-hidden />Not working</span>
  return <span className="verify verify--unknown"><ShieldQuestion aria-hidden />Unverified</span>
}

function CredentialRow({ item, onChanged }: { item: GitCredential; onChanged: () => void }) {
  const [rotating, setRotating] = useState(false)
  const [secret, setSecret] = useState('')
  const [repository, setRepository] = useState(item.verifyRepository ?? '')
  const rotate = useMutation({
    mutationFn: () => api.patch<CredentialResponse>(`/git-credentials/${item.id}`, { name: item.name, username: item.username, host: item.host, secret, repository }),
    onSuccess: () => { setSecret(''); setRotating(false); onChanged() },
  })
  const verify = useMutation({
    mutationFn: () => api.post<CredentialResponse>(`/git-credentials/${item.id}/verify`, { repository }),
    onSuccess: onChanged,
  })
  const remove = useMutation({ mutationFn: () => api.del<void>(`/git-credentials/${item.id}`), onSuccess: onChanged })
  return (
    <li>
      <div className="credential-row">
        <div>
          <strong>{item.name}</strong>
          <small>{item.kind === 'ssh' ? 'SSH deploy key' : `token · ${item.username}`}{item.host ? ` · ${item.host}` : ''}{item.hint ? ` · ${item.hint}` : ''}</small>
          <small><VerifyBadge item={item} />{item.lastUsedAt ? ` · last used ${new Date(item.lastUsedAt).toLocaleDateString()}` : ' · never used'}</small>
          {item.lastVerifyStatus === 'failed' && item.lastVerifyError ? <small className="credential-error">{item.lastVerifyError}</small> : null}
          {item.lastVerifyStatus === 'skipped' ? <small className="credential-error">No repository to test against — this credential will only be tested when a deployment needs it. Add one below.</small> : null}
        </div>
        <div className="button-group">
          <Button variant="secondary" busy={verify.isPending} onClick={() => verify.mutate()}><RefreshCw aria-hidden />Verify</Button>
          <Button variant="secondary" onClick={() => setRotating(value => !value)}><KeyRound aria-hidden />Rotate</Button>
          <Button variant="secondary" busy={remove.isPending} onClick={() => remove.mutate()}><Trash2 aria-hidden />Delete</Button>
        </div>
      </div>
      {rotating ? (
        <form className="credential-form" onSubmit={(event: FormEvent) => { event.preventDefault(); rotate.mutate() }}>
          <div className="form-grid">
            <label className="span-2">{item.kind === 'token' ? 'Replacement token' : 'Replacement private key (PEM)'}
              {item.kind === 'token'
                ? <input required type="password" value={secret} onChange={e => setSecret(e.target.value)} placeholder="ghp_…" autoComplete="off" />
                : <textarea required rows={6} value={secret} onChange={e => setSecret(e.target.value)} />}
              <small>Replaces the secret in place. Every project already using this credential picks up the new one — no re-import, no re-pointing.</small>
            </label>
            <label className="span-2">Repository to verify against<input value={repository} onChange={e => setRepository(e.target.value)} placeholder="https://github.com/owner/repository.git" /></label>
          </div>
          {rotate.error ? <p className="form-error" role="alert">{rotate.error.message}</p> : null}
          <footer className="form-actions">
            <Button type="button" variant="secondary" onClick={() => setRotating(false)}>Cancel</Button>
            <Button type="submit" busy={rotate.isPending}>Rotate and verify</Button>
          </footer>
        </form>
      ) : null}
      {verify.error ? <p className="form-error" role="alert">{verify.error.message}</p> : null}
      {remove.error ? <p className="form-error" role="alert">{remove.error.message}</p> : null}
    </li>
  )
}

export default function GitCredentials() {
  const qc = useQueryClient()
  const query = useQuery({ queryKey: ['git-credentials'], queryFn: () => api.get<{ items: GitCredential[] }>('/git-credentials') })
  const refresh = () => qc.invalidateQueries({ queryKey: ['git-credentials'] })
  const [open, setOpen] = useState(false)
  const [kind, setKind] = useState<Kind>('token')
  const [name, setName] = useState('')
  const [host, setHost] = useState('')
  const [username, setUsername] = useState('')
  const [secret, setSecret] = useState('')
  const [repository, setRepository] = useState('')
  const reset = () => { setName(''); setHost(''); setUsername(''); setSecret(''); setRepository(''); setOpen(false) }
  const create = useMutation({
    mutationFn: () => api.post<CredentialResponse>('/git-credentials', { name, kind, host, username, secret, repository }),
    onSuccess: () => { refresh(); reset() },
  })
  const verifyAll = useMutation({ mutationFn: () => api.post('/git-credentials/verify', {}), onSuccess: refresh })
  const submit = (event: FormEvent) => { event.preventDefault(); create.mutate() }
  const items = query.data?.items ?? []
  const broken = items.filter(item => item.lastVerifyStatus === 'failed')
  return (
    <section className="panel">
      <div className="section-heading">
        <div>
          <p className="eyebrow">SOURCE ACCESS</p>
          <h2>Git credentials</h2>
        </div>
        <KeyRound />
      </div>
      <p className="panel-hint">Secrets are encrypted with a host-local key and are never returned by the API. They are used only while cloning, and never written into a project's source. Each one is re-proven against its repository on a schedule, so a revoked key shows up here rather than in the middle of a release.</p>
      {broken.length > 0 ? <p className="form-error" role="alert">{broken.length === 1 ? `${broken[0].name} cannot reach its repository.` : `${broken.length} credentials cannot reach their repositories.`} Rotate the secret, or check that the key is still authorised on the repository.</p> : null}
      {items.length === 0 ? <p className="empty-hint">No credentials stored. Public repositories import without one.</p> : (
        <ul className="credential-list">
          {items.map(item => <CredentialRow key={item.id} item={item} onChanged={refresh} />)}
        </ul>
      )}
      {items.length > 0 ? <Button variant="secondary" busy={verifyAll.isPending} onClick={() => verifyAll.mutate()}><RefreshCw aria-hidden />Verify all</Button> : null}
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
            <label className="span-2">Repository to verify against<input value={repository} onChange={e => setRepository(e.target.value)} placeholder="https://github.com/owner/repository.git" />
              <small>Asgard runs <code>git ls-remote</code> against this repository now and on every re-check. Token scopes are per repository, so reaching the host proves nothing — without a repository here, a broken credential stays invisible until a deployment needs it.</small>
            </label>
          </div>
          {create.error ? <p className="form-error" role="alert">{create.error.message}</p> : null}
          <footer className="form-actions">
            <Button type="button" variant="secondary" onClick={reset}>Cancel</Button>
            <Button type="submit" busy={create.isPending}>Store and verify</Button>
          </footer>
        </form>
      ) : <Button variant="secondary" onClick={() => setOpen(true)}><Plus aria-hidden />Add credential</Button>}
    </section>
  )
}
