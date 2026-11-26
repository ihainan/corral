// Pure display helpers for subscription tier labels in the quota popover and
// status bar tooltip: normalize provider plan names, omit unknown/placeholder
// strings so the UI never displays "Unknown" / "未知", and compose the heading
// tooltip text.

const PLACEHOLDER_PLANS = new Set(['unknown', 'none', 'null', 'n/a', 'unspecified', '未知'])

export function quotaPlanLabel(plan?: string | null): string | undefined {
  if (typeof plan !== 'string') return undefined
  if (/[\u0000-\u001f\u007f]/.test(plan)) return undefined
  const clean = plan.trim().replace(/\s+/g, ' ')
  if (!clean || clean.length > 80) return undefined
  if (PLACEHOLDER_PLANS.has(clean.toLowerCase())) return undefined
  return clean
}

export function pickQuotaPlan(shown?: { plan?: string } | null, fallback?: string | null): string | undefined {
  return quotaPlanLabel(shown?.plan) ?? quotaPlanLabel(fallback)
}

export function quotaTitleText(title: string, plan?: string | null): string {
  const tier = quotaPlanLabel(plan)
  return tier ? `${title} · ${tier}` : title
}
