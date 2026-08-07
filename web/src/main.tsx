import React from 'react'
import ReactDOM from 'react-dom/client'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { BrowserRouter } from './router'
import App from './App'
import './styles.css'

const queryClient=new QueryClient({defaultOptions:{queries:{staleTime:5_000,retry:(count,error:any)=>error?.status>=500&&count<2,refetchOnWindowFocus:true},mutations:{retry:false}}})

ReactDOM.createRoot(document.getElementById('root')!).render(<React.StrictMode><QueryClientProvider client={queryClient}><BrowserRouter><App/></BrowserRouter></QueryClientProvider></React.StrictMode>)
