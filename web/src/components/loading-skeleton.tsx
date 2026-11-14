import { cn } from "@/lib/utils"

export function LoadingSkeleton({ className = "h-32" }: { className?: string }) {
  return <div className={cn("animate-pulse bg-muted rounded-lg", className)} />
}
