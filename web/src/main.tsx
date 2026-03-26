import { MutationCache, QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { StrictMode } from "react"
import { createRoot } from "react-dom/client"
import { BrowserRouter, Navigate, Route, Routes } from "react-router-dom"
import { ErrorBoundary } from "@/components/error-boundary"
import { AdminLayout, PortalLayout } from "@/components/layout"
import { showToast, ToastBridge, ToastProvider } from "@/components/toast"
import { AuthProvider } from "@/hooks/use-auth"
import { SiteConfigProvider } from "@/hooks/use-site-config"
import { I18nProvider } from "@/i18n"
import AddonsPage from "@/pages/admin/addons"
import AnalyticsPage from "@/pages/admin/analytics"
import APIKeysPage from "@/pages/admin/api-keys"
import AuditPage from "@/pages/admin/audit"
import CustomersPage from "@/pages/admin/customers"
import DashboardPage from "@/pages/admin/dashboard"
import LicensesPage from "@/pages/admin/licenses"
import PlansPage from "@/pages/admin/plans"
import ProductsPage from "@/pages/admin/products"
import SettingsPage from "@/pages/admin/settings"
import WebhooksPage from "@/pages/admin/webhooks"
import LoginPage from "@/pages/login"
import PortalAccountPage from "@/pages/portal/account"
import PortalLicensesPage from "@/pages/portal/licenses"
import "./index.css"

const queryClient = new QueryClient({
  mutationCache: new MutationCache({
    onError: (error) => {
      showToast(error instanceof Error ? error.message : "An error occurred")
    },
  }),
  defaultOptions: {
    queries: { retry: 1, refetchOnWindowFocus: false, staleTime: 30_000 },
  },
})

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <ToastProvider>
          <ToastBridge />
          <I18nProvider>
            <SiteConfigProvider>
              <AuthProvider>
                <ErrorBoundary>
                  <Routes>
                    <Route path="/login" element={<LoginPage />} />

                    {/* Admin */}
                    <Route path="/admin" element={<AdminLayout />}>
                      <Route index element={<DashboardPage />} />
                      <Route path="products" element={<ProductsPage />} />
                      <Route path="plans" element={<PlansPage />} />
                      <Route path="licenses" element={<LicensesPage />} />
                      <Route path="api-keys" element={<APIKeysPage />} />
                      <Route path="webhooks" element={<WebhooksPage />} />
                      <Route path="addons" element={<AddonsPage />} />
                      <Route path="analytics" element={<AnalyticsPage />} />
                      <Route path="audit" element={<AuditPage />} />
                      <Route path="customers" element={<CustomersPage />} />
                      <Route path="settings" element={<SettingsPage />} />
                    </Route>

                    {/* Portal */}
                    <Route path="/portal" element={<PortalLayout />}>
                      <Route index element={<PortalLicensesPage />} />
                      <Route path="account" element={<PortalAccountPage />} />
                    </Route>

                    <Route path="*" element={<Navigate to="/login" replace />} />
                  </Routes>
                </ErrorBoundary>
              </AuthProvider>
            </SiteConfigProvider>
          </I18nProvider>
        </ToastProvider>
      </BrowserRouter>
    </QueryClientProvider>
  </StrictMode>,
)
