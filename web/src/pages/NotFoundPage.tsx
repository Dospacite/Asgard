import { Compass } from 'lucide-react'
import { Link } from '../router'
import { EmptyState } from '../components/ui'
export default function NotFoundPage(){return <div className="page"><EmptyState icon={<Compass/>} title="This route is uncharted" action={<Link className="button button--primary" to="/"><span>Return to overview</span></Link>}>The page may have moved, or the address may be incomplete.</EmptyState></div>}
