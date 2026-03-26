import { Component, type ErrorInfo, type ReactNode } from "react"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { useI18n } from "@/i18n"

interface Props {
  children: ReactNode
  fallback?: ReactNode
}

interface State {
  hasError: boolean
  error: Error | null
}

export class ErrorBoundary extends Component<Props, State> {
  constructor(props: Props) {
    super(props)
    this.state = { hasError: false, error: null }
  }

  static getDerivedStateFromError(error: Error): State {
    return { hasError: true, error }
  }

  componentDidCatch(error: Error, errorInfo: ErrorInfo) {
    console.error("ErrorBoundary caught:", error, errorInfo)
  }

  render() {
    if (this.state.hasError) {
      if (this.props.fallback) return this.props.fallback

      return <ErrorFallback error={this.state.error} onRetry={() => this.setState({ hasError: false, error: null })} />
    }

    return this.props.children
  }
}

function ErrorFallback({ error, onRetry }: { error: Error | null; onRetry: () => void }) {
  const { t } = useI18n()
  return (
    <div className="flex items-center justify-center min-h-[400px] p-8">
      <Card className="max-w-md w-full">
        <CardContent className="pt-6 text-center space-y-4">
          <div className="text-4xl">:(</div>
          <h2 className="text-lg font-semibold">{t("error.title")}</h2>
          <p className="text-sm text-muted-foreground">{error?.message || t("error.unexpected")}</p>
          <div className="flex gap-2 justify-center">
            <Button variant="outline" onClick={onRetry}>
              {t("error.tryAgain")}
            </Button>
            <Button onClick={() => window.location.reload()}>{t("error.reload")}</Button>
          </div>
        </CardContent>
      </Card>
    </div>
  )
}
