import { showToast } from "@/components/toast"

export async function copyText(value: string, failureMessage: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(value)
    return true
  } catch {
    showToast(failureMessage, "error")
    return false
  }
}
