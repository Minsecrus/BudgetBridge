import { Component, type ErrorInfo, type ReactNode } from 'react'

// ErrorBoundary catches render-time errors in its subtree so a single malformed
// data point (e.g. a request-log event carrying an unexpected null field) degrades
// to a small inline fallback instead of white-screening the whole dashboard. React
// error boundaries are the only construct that catches errors thrown during render
// (try/catch can't). Defends the app against any future null/shape regression the
// backend or a third-party payload might throw at it.
interface State { error: Error | null }

export class ErrorBoundary extends Component<{ children: ReactNode }, State> {
  state: State = { error: null }

  static getDerivedStateFromError(error: Error): State {
    return { error }
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error('[BudgetBridge] render error:', error, info)
  }

  render() {
    if (this.state.error) {
      return (
        <div className="min-h-screen bg-ink-950 text-gray-200 flex items-center justify-center p-6">
          <div className="max-w-md text-center">
            <h1 className="text-lg font-semibold text-rose-300 mb-2">页面渲染出错</h1>
            <p className="text-sm text-gray-400 mb-4">
              通常是某条异常数据触发的。刷新一般可恢复；若持续出现请联系管理员。
            </p>
            <pre className="text-xs text-left bg-white/5 border border-white/10 rounded-lg p-3 overflow-auto max-h-48 text-rose-200 whitespace-pre-wrap break-all">
              {this.state.error.message || String(this.state.error)}
            </pre>
            <button
              className="mt-4 px-4 py-2 rounded-md bg-violet-500/20 hover:bg-violet-500/30 text-violet-200 text-sm"
              onClick={() => this.setState({ error: null })}
            >
              重试
            </button>
          </div>
        </div>
      )
    }
    return this.props.children
  }
}
