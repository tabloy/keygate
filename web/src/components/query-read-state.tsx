import type { ReactNode } from "react"
import { Button } from "@/components/ui/button"
import { useI18n } from "@/i18n"
import { errorCode } from "@/lib/api"

type ReadQuery = {
  data: unknown
  isPending: boolean
  isError: boolean
  error: Error | null
  isFetching: boolean
  refetch: () => Promise<unknown>
}

function ReadFailure({ queries, stale }: { queries: ReadQuery[]; stale: boolean }) {
  const { t } = useI18n()
  const missing = queries.some((query) => errorCode(query.error) === "NOT_FOUND")
  return (
    <div role="alert" className="rounded-lg border border-destructive/30 bg-destructive/5 p-4 space-y-2">
      <p className="font-medium">
        {t(stale ? "common.refreshFailed" : missing ? "common.readNotFound" : "common.readFailed")}
      </p>
      <p className="text-sm text-muted-foreground">{queries[0].error?.message}</p>
      <Button
        type="button"
        variant="outline"
        disabled={queries.some((query) => query.isFetching)}
        onClick={() => void Promise.all(queries.map((query) => query.refetch()))}
      >
        {t("common.retry")}
      </Button>
    </div>
  )
}

function PendingRead() {
  const { t } = useI18n()
  return <div role="status" aria-label={t("common.loading")} className="h-32 animate-pulse rounded-lg bg-muted" />
}

export function queryReadState(queries: ReadQuery[], waitFor: ReadQuery[] = []) {
  const failed = queries.filter((query) => query.isError)
  const coldFailure = failed.some((query) => query.data === undefined)
  const notice = failed.length > 0 ? <ReadFailure queries={failed} stale={!coldFailure} /> : null
  return {
    blocked: coldFailure ? (
      notice
    ) : waitFor.some((query) => query.isPending && query.data === undefined) ? (
      <PendingRead />
    ) : null,
    notice: coldFailure ? null : notice,
  }
}

export function QueryReadState({ queries, children }: { queries: ReadQuery[]; children: ReactNode }) {
  const state = queryReadState(queries, queries)
  return (
    state.blocked ?? (
      <>
        {state.notice}
        {children}
      </>
    )
  )
}
