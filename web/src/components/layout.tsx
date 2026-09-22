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
  Menu,
  Package,
  Rocket,
  ScrollText,
  Settings,
  User,
  Users,
} from "lucide-react"
import { useState } from "react"
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
import { Sheet, SheetContent, SheetTrigger } from "@/components/ui/sheet"
import { useAuth } from "@/hooks/use-auth"
import { useSiteConfig } from "@/hooks/use-site-config"
import { useI18n } from "@/i18n"
import { cn } from "@/lib/utils"

export function AdminLayout() {
  const { user, loading, logout } = useAuth()
  const { site_name, logo_url, attribution_text, attribution_url } = useSiteConfig()
  const { t } = useI18n()
  const [navOpen, setNavOpen] = useState(false)

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
        { to: "/admin/releases", label: t("nav.releases"), icon: Rocket },
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

  // Which nav entry the current URL belongs to. Same rule the sidebar
  // highlights with, so the two can never disagree about where you
  // are.
  const isActive = (to: string) => (to === "/admin" ? location.pathname === "/admin" : location.pathname.startsWith(to))

  if (loading) return <LoadingScreen />
  if (!user) return <Navigate to="/login" replace />
  if (!user.is_admin) return <Navigate to="/portal" replace />

  // Settings sits below the groups rather than in one, so it is built
  // here and rendered on its own further down.
  const settingsItem: NavItem = { to: "/admin/settings", label: t("nav.settings"), icon: Settings }

  // Which section the narrow-screen bar names. The label comes from
  // the nav list rather than being written out again, so a renamed
  // section renames here too. Longest path first: /admin matches
  // everything, so it has to lose to /admin/licenses when both fit.
  const currentPage = [...adminNav.flatMap((e) => ("to" in e ? [e] : e.items)), settingsItem]
    .sort((a, b) => b.to.length - a.to.length)
    .find((i) => isActive(i.to))

  const renderNavItem = (item: NavItem) => {
    const active = isActive(item.to)
    return (
      // Tapping a link inside the drawer navigates and closes it; on
      // a wide screen there is no drawer and the state is inert.
      <Link key={item.to} to={item.to} onClick={() => setNavOpen(false)}>
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

  // The sidebar's contents are built once and mounted twice: as the
  // permanent column on a wide screen, and inside the drawer on a
  // narrow one. Two copies of this markup would drift apart the first
  // time a nav entry was added to one of them.
  const sidebar = (
    <>
      {/* pr-11 below md leaves room for the drawer's close button,
        which sits at the panel's top right. The permanent sidebar is
        hidden below md, so the wider padding only ever applies inside
        the drawer. */}
      <div className="p-4 pr-11 md:pr-4 flex items-center gap-2.5">
        <img src={logo_url || "/logo.svg"} alt={site_name} className="h-7 w-7" />
        <span className="font-bold text-lg tracking-tight truncate">{site_name}</span>
        <span className="text-xs bg-primary/10 text-primary px-1.5 py-0.5 rounded font-medium ml-auto shrink-0">
          Admin
        </span>
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
      <div className="px-2">{renderNavItem(settingsItem)}</div>
      <a href="https://keygate.app/sponsorships" target="_blank" rel="noopener noreferrer" className="block px-2 pb-1">
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
              <div className="h-6 w-6 rounded-full bg-primary text-primary-foreground flex items-center justify-center text-xs font-bold shrink-0">
                {user.name?.charAt(0)?.toUpperCase() || user.email.charAt(0).toUpperCase()}
              </div>
              <span className="truncate text-sm">{user.name || user.email}</span>
              <ChevronDown className="h-4 w-4 ml-auto opacity-50 shrink-0" />
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
    </>
  )

  return (
    <div className="flex h-screen bg-background">
      {/* Sidebar — a column of its own from md up, and the contents of
        the drawer below it. 240px of a 390px phone is not a sidebar,
        it is the whole screen. */}
      <aside className="hidden w-60 shrink-0 border-r bg-card md:flex md:flex-col">{sidebar}</aside>

      {/* min-w-0 is what lets a wide table scroll inside this column
        instead of stretching it: a flex child's default min-width is
        its content, so without it the page grows to the width of the
        table and the whole layout scrolls sideways. */}
      <div className="flex min-w-0 flex-1 flex-col">
        <header className="flex h-14 shrink-0 items-center gap-2 border-b bg-card px-2 md:hidden">
          <Sheet open={navOpen} onOpenChange={setNavOpen}>
            <SheetTrigger asChild>
              {/* 44px square: the size a finger needs. The icon inside
                stays 20px, so only the hit area grows. */}
              <Button variant="ghost" size="icon" className="h-11 w-11 shrink-0" aria-label={t("nav.menu")}>
                <Menu className="h-5 w-5" />
              </Button>
            </SheetTrigger>
            <SheetContent side="left" label={t("nav.menu")}>
              {sidebar}
            </SheetContent>
          </Sheet>
          {/* The mark carries the brand and the words carry the
            location. Spelling the site name out here as well would
            just repeat what the drawer says two taps away, on the one
            screen with no room to spare. */}
          {/* The link carries the accessible name, so the image inside
            it is decorative and must not announce the same thing
            again. */}
          <Link to="/admin" className="shrink-0" aria-label={site_name}>
            <img src={logo_url || "/logo.svg"} alt="" className="h-6 w-6" />
          </Link>
          <span className="min-w-0 flex-1 truncate font-semibold tracking-tight">
            {currentPage?.label ?? site_name}
          </span>
        </header>

        <main className="flex-1 overflow-auto">
          <div className="p-4 md:p-8">
            <ErrorBoundary>
              <Outlet />
            </ErrorBoundary>
          </div>
        </main>

        {/* Attribution required by AGPL v3 Section 7(b) — see NOTICE.
          The sidebar carries it on a wide screen, but the sidebar is
          hidden below md and its copy inside the drawer is only on
          screen while the drawer is open. This bar is the narrow
          screen's permanent surface for it: outside <main>, so it
          does not scroll away, and shown only where the sidebar is
          not. */}
        <div className="shrink-0 border-t bg-card px-4 py-1.5 text-center md:hidden">
          <a
            href={attribution_url}
            target="_blank"
            rel="noopener noreferrer"
            className="text-[10px] text-muted-foreground/60 hover:text-muted-foreground transition-colors"
          >
            {attribution_text}
          </a>
        </div>
      </div>
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
