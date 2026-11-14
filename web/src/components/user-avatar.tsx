import { cn } from "@/lib/utils"

type AvatarSize = "xs" | "sm" | "md" | "lg" | "xl"

const sizeClasses: Record<AvatarSize, { container: string; text: string }> = {
  xs: { container: "h-6 w-6", text: "text-xs" },
  sm: { container: "h-8 w-8", text: "text-xs" },
  md: { container: "h-10 w-10", text: "text-sm" },
  lg: { container: "h-14 w-14", text: "text-lg" },
  xl: { container: "h-16 w-16", text: "text-xl" },
}

export function UserAvatar({
  user,
  size = "sm",
  variant = "muted",
  className,
}: {
  user?: { name?: string | null; email?: string | null; avatar_url?: string | null } | null
  size?: AvatarSize
  variant?: "muted" | "primary"
  className?: string
}) {
  const { container, text } = sizeClasses[size]
  const initial = user?.name?.charAt(0)?.toUpperCase() || user?.email?.charAt(0)?.toUpperCase() || "?"

  if (user?.avatar_url) {
    return (
      <img src={user.avatar_url} className={cn(container, "rounded-full object-cover shrink-0", className)} alt="" />
    )
  }

  const bgClass = variant === "primary" ? "bg-primary text-primary-foreground" : "bg-muted text-muted-foreground"

  return (
    <div
      className={cn(
        container,
        "rounded-full flex items-center justify-center font-bold shrink-0",
        text,
        bgClass,
        className,
      )}
    >
      {initial}
    </div>
  )
}
