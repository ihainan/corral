export interface ParsedApiError {
  status?: string
  operation?: string
  headline: string
  requestIds: string[]
  raw: string
}

// Recognizes synthetic upstream API errors emitted directly as assistant text
// (e.g. Claude Code's "API Error: 400 InvokeModelWithResponseStream: ...") and
// parses them into a concise, scannable error summary plus deduplicated request IDs.
export function parseApiError(text: string): ParsedApiError | null {
  const raw = text.trim()
  if (!raw || !/^API\s+Error\s*:/i.test(raw)) return null
  // Do not hijack multi-paragraph explanations or markdown blocks that happen to quote an error.
  if (raw.includes('```') || raw.split('\n').filter((l) => l.trim()).length > 6) return null

  const rest = raw.replace(/^API\s+Error\s*:\s*/i, '').trim()
  if (!rest) return null

  let body = rest
  let status: string | undefined
  const statusMatch = /^(\d{3})\b\s*[:,-]?\s*(.*)$/s.exec(body)
  if (statusMatch) {
    status = statusMatch[1]
    body = statusMatch[2].trim()
  }

  // Extract deduplicated request IDs from both AWS/Bedrock ("RequestID: ...") and proxy ("(request id: ...)").
  const requestIds: string[] = []
  const seen = new Set<string>()
  const addId = (id: string) => {
    const clean = id.trim()
    if (!clean || seen.has(clean)) return
    seen.add(clean)
    requestIds.push(clean)
  }

  body = body.replace(/\(\s*request[\s_-]*id\s*[:=]\s*([^)\s]+)\s*\)/gi, (_m, id: string) => {
    addId(id)
    return ''
  })
  body = body.replace(/,?\s*\bRequestID\s*[:=]\s*([A-Za-z0-9_.:-]+)\s*,?/gi, (_m, id: string) => {
    addId(id)
    return ', '
  })
  body = body.replace(/\s+/g, ' ').replace(/,\s*,+/g, ', ').replace(/^[,:\s]+|[,:\s]+$/g, '').trim()

  // Extract embedded JSON error payload if present (e.g. Anthropic / OpenAI JSON error).
  const jsonStart = body.indexOf('{')
  if (jsonStart >= 0) {
    try {
      const parsed = JSON.parse(body.slice(jsonStart)) as {
        error?: { type?: string; message?: string; code?: string }
        message?: string
      }
      const errMsg = parsed?.error?.message || parsed?.message
      if (typeof errMsg === 'string' && errMsg.trim()) {
        const prefix = body.slice(0, jsonStart).replace(/[:\s-]+$/, '').trim()
        const errType = parsed?.error?.type || parsed?.error?.code
        const headline = errType ? `${errType}: ${errMsg.trim()}` : errMsg.trim()
        return {
          status,
          operation: prefix || undefined,
          headline,
          requestIds,
          raw,
        }
      }
    } catch {
      /* not valid JSON; continue with text heuristics */
    }
  }

  // Extract AWS / Bedrock Go SDK wrapped errors:
  // "InvokeModelWithResponseStream: operation error Bedrock Runtime: InvokeModelWithResponseStream, https response error StatusCode: 400, ValidationException: ..."
  let operation: string | undefined
  const opMatch = /^(?:([A-Za-z0-9_.-]+)\s*:\s*)?operation error\s+([^:,]+?)\s*:\s*([^,]+),\s*(.*)$/i.exec(body)
  if (opMatch) {
    const service = opMatch[2].trim()
    const op = opMatch[3].trim()
    operation = service && op ? `${service} · ${op}` : service || op
    body = opMatch[4].trim()
  }

  // Strip redundant "https response error StatusCode: 400," once status is already captured.
  body = body.replace(/^https?\s+response\s+error\s+StatusCode\s*:\s*(\d{3})\s*,?\s*/i, (_m, code: string) => {
    if (!status) status = code
    return ''
  })
  body = body.replace(/^[,:\s]+|[,:\s]+$/g, '').trim()

  return {
    status,
    operation,
    headline: body || rest,
    requestIds,
    raw,
  }
}
