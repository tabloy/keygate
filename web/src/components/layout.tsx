import {
  BarChart3,
  Blocks,
  ChevronDown,
  FileKey2,
  Heart,
  Key,
  Layers,
  LayoutDashboard,
  Link2,
  LogOut,
  Package,
  ScrollText,
  Settings,
  User,
  Users,
} from "lucide-react"
import { Link, Navigate, Outlet, useLocation } from "react-router-dom"
import { ErrorBoundary } from "@/components/error-boundary"
import { Button } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Separator } from "@/components/ui/separator"
import { useAuth } from "@/hooks/use-auth"
import { useSiteConfig } from "@/hooks/use-site-config"
import { useI18n } from "@/i18n"
import { cn } from "@/lib/utils"

export function AdminLayout() {
  const { user, loading, logout } = useAuth()
  const { site_name, logo_url, attribution_text, attribution_url } = useSiteConfig()
  const { t } = useI18n()

  type NavItem = { to: string; label: string; icon: React.ComponentType<{ className?: string }> }
  type NavGroup = { label: string; items: NavItem[] }

  const adminNav: (NavItem | NavGroup)[] = [
    { to: "/admin", label: t("nav.dashboard"), icon: LayoutDashboard },
    {
      label: t("nav.catalog"),
      items: [
        { to: "/admin/products", label: t("nav.products"), icon: Package },
        { to: "/admin/plans", label: t("nav.plans"), icon: Layers },
        { to: "/admin/addons", label: t("nav.addons"), icon: Blocks },
      ],
    },
    {
      label: t("nav.licensing"),
      items: [
        { to: "/admin/licenses", label: t("nav.licenses"), icon: Key },
        { to: "/admin/customers", label: t("nav.customers"), icon: Users },
      ],
    },
    {
      label: t("nav.developer"),
      items: [
        { to: "/admin/api-keys", label: t("nav.apiKeys"), icon: FileKey2 },
        { to: "/admin/webhooks", label: t("nav.webhooks"), icon: Link2 },
      ],
    },
    {
      label: t("nav.insights"),
      items: [
        { to: "/admin/analytics", label: t("nav.analytics"), icon: BarChart3 },
        { to: "/admin/audit", label: t("nav.audit"), icon: ScrollText },
      ],
    },
  ]
  const location = useLocation()

  if (loading) return <LoadingScreen />
  if (!user) return <Navigate to="/login" replace />
  if (!user.is_admin) return <Navigate to="/portal" replace />

  const renderNavItem = (item: NavItem) => {
    const active = item.to === "/admin" ? location.pathname === "/admin" : location.pathname.startsWith(item.to)
    return (
      <Link key={item.to} to={item.to}>
        <div
          className={cn(
            "flex items-center gap-3 rounded-md px-3 py-2 text-sm font-medium transition-colors",
            active
              ? "bg-accent text-accent-foreground"
              : "text-muted-foreground hover:bg-accent hover:text-accent-foreground",
          )}
        >
          <item.icon className="h-4 w-4" />
          {item.label}
        </div>
      </Link>
    )
  }

  return (
    <div className="flex h-screen bg-background">
      {/* Sidebar */}
      <aside className="w-60 border-r bg-card flex flex-col">
        <div className="p-4 flex items-center gap-2.5">
          <img src={logo_url || "/logo.svg"} alt={site_name} className="h-7 w-7" />
          <span className="font-bold text-lg tracking-tight">{site_name}</span>
          <span className="text-xs bg-primary/10 text-primary px-1.5 py-0.5 rounded font-medium ml-auto">Admin</span>
        </div>
        <Separator />
        <nav className="flex-1 p-2 space-y-0.5 overflow-y-auto">
          {adminNav.map((entry, idx) => {
            if ("to" in entry) return renderNavItem(entry)
            const group = entry as NavGroup
            return (
              <div key={group.label} className={cn(idx > 0 && "mt-4")}>
                <div className="px-3 py-1.5 text-xs font-semibold text-muted-foreground/60 uppercase tracking-wider">
                  {group.label}
                </div>
                {group.items.map(renderNavItem)}
              </div>
            )
          })}
        </nav>
        {/* Settings — fixed at bottom above user menu */}
        {renderNavItem({ to: "/admin/settings", label: t("nav.settings"), icon: Settings })}
        <a href="https://keygate.app/sponsorships" target="_blank" rel="noopener noreferrer" className="block">
          <div className="flex items-center gap-3 rounded-md px-3 py-2 text-sm font-medium text-muted-foreground hover:bg-pink-50 hover:text-pink-700 transition-colors">
            <Heart className="h-4 w-4 text-pink-500 fill-pink-500" />
            {t("nav.sponsor")}
          </div>
        </a>
        <Separator />
        <div className="p-3">
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button variant="ghost" className="w-full justify-start gap-2">
                <div className="h-6 w-6 rounded-full bg-primary text-primary-foreground flex items-center justify-center text-xs font-bold">
                  {user.name?.charAt(0)?.toUpperCase() || user.email.charAt(0).toUpperCase()}
                </div>
                <span className="truncate text-sm">{user.name || user.email}</span>
                <ChevronDown className="h-4 w-4 ml-auto opacity-50" />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end" className="w-48">
              <DropdownMenuItem className="text-xs text-muted-foreground">{user.email}</DropdownMenuItem>
              <DropdownMenuSeparator />
              <DropdownMenuItem asChild>
                <Link to="/portal">{t("nav.portal")}</Link>
              </DropdownMenuItem>
              <DropdownMenuSeparator />
              <DropdownMenuItem onClick={logout} className="text-destructive">
                <LogOut className="h-4 w-4 mr-2" /> {t("nav.logout")}
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
        {/* Attribution required by AGPL v3 Section 7(b) — see NOTICE */}
        <div className="px-4 py-2 border-t text-center">
          <a
            href={attribution_url}
            target="_blank"
            rel="noopener noreferrer"
            className="text-[10px] text-muted-foreground/50 hover:text-muted-foreground transition-colors"
          >
            {attribution_text}
          </a>
        </div>
      </aside>

      {/* Main */}
      <main className="flex-1 overflow-auto">
        <div className="p-8">
          <ErrorBoundary>
            <Outlet />
          </ErrorBoundary>
        </div>
      </main>
    </div>
  )
}

export function PortalLayout() {
  const { user, loading, logout } = useAuth()
  const { site_name, logo_url, attribution_text, attribution_url } = useSiteConfig()
  const { t } = useI18n()

  const portalNav = [
    { to: "/portal", label: t("nav.licenses"), icon: Key },
    { to: "/portal/account", label: t("nav.settings"), icon: User },
  ]
  const location = useLocation()

  if (loading) return <LoadingScreen />
  if (!user) return <Navigate to="/login" replace />

  return (
    <div className="min-h-screen bg-background">
      <header className="border-b bg-card">
        <div className="max-w-5xl mx-auto flex items-center justify-between h-14 px-4">
          <Link to="/portal" className="flex items-center gap-2 font-bold text-lg tracking-tight">
            <img src={logo_url || "/logo.svg"} alt={site_name} className="h-6 w-6" />
            {site_name}
          </Link>
          <div className="flex items-center gap-4">
            {user.is_admin && (
              <Link to="/admin">
                <Button variant="outline" size="sm">
                  Admin Panel
                </Button>
              </Link>
            )}
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button variant="ghost" size="sm" className="gap-2">
                  <User className="h-4 w-4" />
                  {user.name || user.email}
                  <ChevronDown className="h-3 w-3 opacity-50" />
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end">
                <DropdownMenuItem className="text-xs text-muted-foreground">{user.email}</DropdownMenuItem>
                <DropdownMenuSeparator />
                <DropdownMenuItem onClick={logout} className="text-destructive">
                  <LogOut className="h-4 w-4 mr-2" /> {t("nav.logout")}
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          </div>
        </div>
        {/* Portal navigation */}
        <div className="max-w-5xl mx-auto px-4">
          <nav className="flex gap-1 -mb-px">
            {portalNav.map((item) => {
              const active =
                item.to === "/portal" ? location.pathname === "/portal" : location.pathname.startsWith(item.to)
              return (
                <Link key={item.to} to={item.to}>
                  <div
                    className={cn(
                      "flex items-center gap-2 px-3 py-2 text-sm font-medium border-b-2 transition-colors",
                      active
                        ? "border-primary text-primary"
                        : "border-transparent text-muted-foreground hover:text-foreground hover:border-muted-foreground/30",
                    )}
                  >
                    <item.icon className="h-4 w-4" />
                    {item.label}
                  </div>
                </Link>
              )
            })}
          </nav>
        </div>
      </header>
      <main className="max-w-5xl mx-auto p-4 md:p-8">
        <ErrorBoundary>
          <Outlet />
        </ErrorBoundary>
      </main>
      {/* Attribution required by AGPL v3 Section 7(b) — see NOTICE */}
      <footer className="border-t py-3 text-center">
        <a
          href={attribution_url}
          target="_blank"
          rel="noopener noreferrer"
          className="text-xs text-muted-foreground/50 hover:text-muted-foreground transition-colors"
        >
          {attribution_text}
        </a>
      </footer>
    </div>
  )
}

function LoadingScreen() {
  return (
    <div className="flex items-center justify-center h-screen">
      <div className="animate-spin h-8 w-8 border-4 border-primary border-t-transparent rounded-full" />
    </div>
  )
}
