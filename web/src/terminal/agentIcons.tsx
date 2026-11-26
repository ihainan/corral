// The icons of the agents a pane may run, by the name herdr gives the
// program (claude, codex, ...): a monochrome mark on a tile, in its maker's
// colour where it has one (else the text's). Another program gets a
// terminal's mark.
//
// The marks' paths are from Lobe Icons (https://github.com/lobehub/lobe-icons),
// MIT License, Copyright (c) 2023 LobeHub. The marks themselves are their
// owners' trademarks, shown here only to say which program runs.
import { useId } from 'react'
import { Cpu, SquareTerminal } from 'lucide-react'

// grad: the mark is painted in its maker's colours down its height (their
// colour icon, where the mono one would be a black blob); color is then
// the one the tile is tinted in.
type Mark = { name: string; color?: string; grad?: string[]; d: string; even?: boolean; multi?: 'agy' }

const AGY_MASK_D =
  'M21.751 22.607c1.34 1.005 3.35.335 1.508-1.508C17.73 15.74 18.904 1 12.037 1 5.17 1 6.342 15.74.815 21.1c-2.01 2.009.167 2.511 1.507 1.506 5.192-3.517 4.857-9.714 9.715-9.714 4.857 0 4.522 6.197 9.714 9.715z'

const AGY_BLOBS: { d: string; fill: string; x: number; y: number; w: number; h: number; blur: number }[] = [
  { d: 'M-1.018-3.992c-.408 3.591 2.686 6.89 6.91 7.37 4.225.48 7.98-2.043 8.387-5.633.408-3.59-2.686-6.89-6.91-7.37-4.225-.479-7.98 2.043-8.387 5.633z', fill: '#FFE432', x: -3.288, y: -11.917, w: 19.838, h: 17.587, blur: 1.117 },
  { d: 'M15.269 7.747c1.058 4.557 5.691 7.374 10.348 6.293 4.657-1.082 7.575-5.653 6.516-10.21-1.058-4.556-5.691-7.374-10.348-6.292-4.657 1.082-7.575 5.653-6.516 10.21z', fill: '#FC413D', x: 4.251, y: -13.493, w: 38.9, h: 38.565, blur: 5.4 },
  { d: 'M-12.443 10.804c1.338 4.703 7.36 7.11 13.453 5.378 6.092-1.733 9.947-6.95 8.61-11.652C8.282-.173 2.26-2.58-3.833-.848-9.925.884-13.78 6.1-12.443 10.804z', fill: '#00B95C', x: -21.889, y: -10.592, w: 40.955, h: 36.517, blur: 4.591 },
  { d: 'M-12.443 10.804c1.338 4.703 7.36 7.11 13.453 5.378 6.092-1.733 9.947-6.95 8.61-11.652C8.282-.173 2.26-2.58-3.833-.848-9.925.884-13.78 6.1-12.443 10.804z', fill: '#00B95C', x: -21.889, y: -10.592, w: 40.955, h: 36.517, blur: 4.591 },
  { d: 'M-7.608 14.703c3.352 3.424 9.126 3.208 12.896-.483 3.77-3.69 4.108-9.459.756-12.883C2.69-2.087-3.083-1.871-6.853 1.82c-3.77 3.69-4.108 9.458-.755 12.883z', fill: '#00B95C', x: -19.099, y: -10.278, w: 36.632, h: 36.595, blur: 4.591 },
  { d: 'M9.932 27.617c1.04 4.482 5.384 7.303 9.7 6.3 4.316-1.002 6.971-5.448 5.93-9.93-1.04-4.483-5.384-7.304-9.7-6.301-4.316 1.002-6.971 5.448-5.93 9.93z', fill: '#3186FF', x: 0.981, y: 8.758, w: 33.533, h: 34.087, blur: 4.363 },
  { d: 'M2.572-8.185C.392-3.329 2.778 2.472 7.9 4.771c5.122 2.3 11.042.227 13.222-4.63 2.18-4.855-.205-10.656-5.327-12.955-5.122-2.3-11.042-.227-13.222 4.63z', fill: '#FBBC04', x: -6.143, y: -21.659, w: 35.978, h: 35.276, blur: 3.954 },
  { d: 'M-3.267 38.686c-5.277-2.072 3.742-19.117 5.984-24.83 2.243-5.712 8.34-8.664 13.616-6.592 5.278 2.071 11.533 13.482 9.29 19.195-2.242 5.713-23.613 14.298-28.89 12.227z', fill: '#3186FF', x: -11.96, y: -0.46, w: 45.114, h: 46.523, blur: 3.531 },
  { d: 'M28.71 17.471c-1.413 1.649-5.1.808-8.236-1.878-3.135-2.687-4.531-6.201-3.118-7.85 1.412-1.649 5.1-.808 8.235 1.878s4.532 6.2 3.119 7.85z', fill: '#749BFF', x: 10.485, y: 0.58, w: 25.094, h: 24.054, blur: 3.159 },
  { d: 'M18.163 9.077c5.81 3.93 12.502 4.19 14.946.577 2.443-3.612-.287-9.727-6.098-13.658-5.81-3.931-12.502-4.19-14.946-.577-2.443 3.612.287 9.727 6.098 13.658z', fill: '#FC413D', x: 5.833, y: -12.467, w: 33.508, h: 30.007, blur: 2.669 },
  { d: 'M-.915 2.684c-1.44 3.473-.97 6.967 1.05 7.804 2.02.837 4.824-1.3 6.264-4.772 1.44-3.473.97-6.967-1.05-7.804-2.02-.837-4.824 1.3-6.264 4.772z', fill: '#FFEE48', x: -8.355, y: -8.876, w: 22.194, h: 26.151, blur: 3.303 },
]

const MARKS: Record<string, Mark> = {
  claude: { name: 'Claude', color: '#D97757', d: 'M4.709 15.955l4.72-2.647.08-.23-.08-.128H9.2l-.79-.048-2.698-.073-2.339-.097-2.266-.122-.571-.121L0 11.784l.055-.352.48-.321.686.06 1.52.103 2.278.158 1.652.097 2.449.255h.389l.055-.157-.134-.098-.103-.097-2.358-1.596-2.552-1.688-1.336-.972-.724-.491-.364-.462-.158-1.008.656-.722.881.06.225.061.893.686 1.908 1.476 2.491 1.833.365.304.145-.103.019-.073-.164-.274-1.355-2.446-1.446-2.49-.644-1.032-.17-.619a2.97 2.97 0 01-.104-.729L6.283.134 6.696 0l.996.134.42.364.62 1.414 1.002 2.229 1.555 3.03.456.898.243.832.091.255h.158V9.01l.128-1.706.237-2.095.23-2.695.08-.76.376-.91.747-.492.584.28.48.685-.067.444-.286 1.851-.559 2.903-.364 1.942h.212l.243-.242.985-1.306 1.652-2.064.73-.82.85-.904.547-.431h1.033l.76 1.129-.34 1.166-1.064 1.347-.881 1.142-1.264 1.7-.79 1.36.073.11.188-.02 2.856-.606 1.543-.28 1.841-.315.833.388.091.395-.328.807-1.969.486-2.309.462-3.439.813-.042.03.049.061 1.549.146.662.036h1.622l3.02.225.79.522.474.638-.079.485-1.215.62-1.64-.389-3.829-.91-1.312-.329h-.182v.11l1.093 1.068 2.006 1.81 2.509 2.33.127.578-.322.455-.34-.049-2.205-1.657-.851-.747-1.926-1.62h-.128v.17l.444.649 2.345 3.521.122 1.08-.17.353-.608.213-.668-.122-1.374-1.925-1.415-2.167-1.143-1.943-.14.08-.674 7.254-.316.37-.729.28-.607-.461-.322-.747.322-1.476.389-1.924.315-1.53.286-1.9.17-.632-.012-.042-.14.018-1.434 1.967-2.18 2.945-1.726 1.845-.414.164-.717-.37.067-.662.401-.589 2.388-3.036 1.44-1.882.93-1.086-.006-.158h-.055L4.132 18.56l-1.13.146-.487-.456.061-.746.231-.243 1.908-1.312-.006.006z', even: true },
  codex: { name: 'Codex', color: '#3941FF', grad: ['#B1A7FF', '#7A9DFF', '#3941FF'], d: 'M8.086.457a6.105 6.105 0 013.046-.415c1.333.153 2.521.72 3.564 1.7a.117.117 0 00.107.029c1.408-.346 2.762-.224 4.061.366l.063.03.154.076c1.357.703 2.33 1.77 2.918 3.198.278.679.418 1.388.421 2.126a5.655 5.655 0 01-.18 1.631.167.167 0 00.04.155 5.982 5.982 0 011.578 2.891c.385 1.901-.01 3.615-1.183 5.14l-.182.22a6.063 6.063 0 01-2.934 1.851.162.162 0 00-.108.102c-.255.736-.511 1.364-.987 1.992-1.199 1.582-2.962 2.462-4.948 2.451-1.583-.008-2.986-.587-4.21-1.736a.145.145 0 00-.14-.032c-.518.167-1.04.191-1.604.185a5.924 5.924 0 01-2.595-.622 6.058 6.058 0 01-2.146-1.781c-.203-.269-.404-.522-.551-.821a7.74 7.74 0 01-.495-1.283 6.11 6.11 0 01-.017-3.064.166.166 0 00.008-.074.115.115 0 00-.037-.064 5.958 5.958 0 01-1.38-2.202 5.196 5.196 0 01-.333-1.589 6.915 6.915 0 01.188-2.132c.45-1.484 1.309-2.648 2.577-3.493.282-.188.55-.334.802-.438.286-.12.573-.22.861-.304a.129.129 0 00.087-.087A6.016 6.016 0 015.635 2.31C6.315 1.464 7.132.846 8.086.457zm-.804 7.85a.848.848 0 00-1.473.842l1.694 2.965-1.688 2.848a.849.849 0 001.46.864l1.94-3.272a.849.849 0 00.007-.854l-1.94-3.393zm5.446 6.24a.849.849 0 000 1.695h4.848a.849.849 0 000-1.696h-4.848z', even: true },
  gemini: { name: 'Gemini', color: '#3186FF', d: 'M20.616 10.835a14.147 14.147 0 01-4.45-3.001 14.111 14.111 0 01-3.678-6.452.503.503 0 00-.975 0 14.134 14.134 0 01-3.679 6.452 14.155 14.155 0 01-4.45 3.001c-.65.28-1.318.505-2.002.678a.502.502 0 000 .975c.684.172 1.35.397 2.002.677a14.147 14.147 0 014.45 3.001 14.112 14.112 0 013.679 6.453.502.502 0 00.975 0c.172-.685.397-1.351.677-2.003a14.145 14.145 0 013.001-4.45 14.113 14.113 0 016.453-3.678.503.503 0 000-.975 13.245 13.245 0 01-2.003-.678z', even: true },
  opencode: { name: 'OpenCode', d: 'M16 6H8v12h8V6zm4 16H4V2h16v20z', even: true },
  // (A stand-in, a plain D: no mark of Devin's own is in the repository.)
  devin: { name: 'Devin', d: 'M5 3h7a9 9 0 010 18H5V3zm3 3v12h4a6 6 0 000-12H8z', even: true },
  agy: { name: 'Antigravity', color: '#3186FF', d: AGY_MASK_D, even: true, multi: 'agy' },
  qwen: { name: 'Qwen', color: '#615CED', d: 'M19.713 2.825h-5.907c0-.251-.066-.503-.196-.728L12.819.728A1.46 1.46 0 0 0 11.559 0h-1.58c-.52 0-1 .275-1.26.728l-2.42 4.19a1.45 1.45 0 0 0 0 1.453l.791 1.37c.26.448.74.727 1.26.727h11.36c.52 0 1-.275 1.26-.728l.79-1.37a1.45 1.45 0 0 0 0-1.451l-.79-1.37a1.46 1.46 0 0 0-1.26-.728zM.195 9.905l2.951 5.115a1.46 1.46 0 0 0-.531.531l-.791 1.37a1.45 1.45 0 0 0 0 1.452l.79 1.37c.26.448.74.727 1.26.727h4.84c.52 0 1-.275 1.26-.728l.79-1.37a1.45 1.45 0 0 0 0-1.451L5.086 7.083a1.46 1.46 0 0 0-1.26-.728h-1.58c-.52 0-1 .276-1.26.728l-.79 1.37a1.45 1.45 0 0 0 0 1.452m15.888 13.367 2.951-5.115c.217.125.469.196.728.196h1.582c.52 0 1-.275 1.26-.728l.79-1.37a1.45 1.45 0 0 0 0-1.451l-2.42-4.191a1.46 1.46 0 0 0-1.259-.728h-1.582c-.52 0-1 .276-1.259.728l-5.678 9.838a1.45 1.45 0 0 0 0 1.452l.79 1.37c.26.448.74.727 1.26.727h1.582c.52 0 1-.275 1.26-.728z', even: true },
  cursor: { name: 'Cursor', d: 'M22.106 5.68L12.5.135a.998.998 0 00-.998 0L1.893 5.68a.84.84 0 00-.419.726v11.186c0 .3.16.577.42.727l9.607 5.547a.999.999 0 00.998 0l9.608-5.547a.84.84 0 00.42-.727V6.407a.84.84 0 00-.42-.726zm-.603 1.176L12.228 22.92c-.063.108-.228.064-.228-.061V12.34a.59.59 0 00-.295-.51l-9.11-5.26c-.107-.062-.063-.228.062-.228h18.55c.264 0 .428.286.296.514z', even: true },
}

// Which of them a program is: its name, or one that has it in it
// ("claude-code", "codex-cli").
export function agentMark(program?: string): Mark | undefined {
  if (!program) return undefined
  const p = program.toLowerCase()
  if (MARKS[p]) return MARKS[p]
  if (p.includes('antigravity')) return MARKS.agy
  const k = Object.keys(MARKS).find((m) => p.includes(m))
  return k ? MARKS[k] : undefined
}

// The tile: size, its side in px. dot: a status to mark on its corner
// (waiting for you, working, done), none at rest. Named for a screen
// reader as the program (the rows say it nowhere else).
// A maker's mark alone, no tile: beside its name, as in a choice of which
// agent to start. undefined for a program with no mark.
export function AgentGlyph({ program, size = 16 }: { program: string; size?: number }) {
  const m = agentMark(program)
  const uid = useId() // (a page holds many of these: each its own)
  if (!m) return null
  if (m.multi === 'agy') {
    const maskId = `${uid}-m`
    return (
      <svg className="agent-glyph" viewBox="0 0 24 24" width={size} height={size} aria-hidden="true">
        <defs>
          <mask id={maskId} maskUnits="userSpaceOnUse" x="0" y="1" width="24" height="23">
            <path d={AGY_MASK_D} fill="#fff" />
          </mask>
          {AGY_BLOBS.map((b, i) => (
            <filter key={i} id={`${uid}-f${i}`} x={b.x} y={b.y} width={b.w} height={b.h} filterUnits="userSpaceOnUse" colorInterpolationFilters="sRGB">
              <feFlood floodOpacity="0" result="BackgroundImageFix" />
              <feBlend in="SourceGraphic" in2="BackgroundImageFix" result="shape" />
              <feGaussianBlur stdDeviation={b.blur} />
            </filter>
          ))}
        </defs>
        <g mask={`url(#${maskId})`}>
          {AGY_BLOBS.map((b, i) => (
            <g key={i} filter={`url(#${uid}-f${i})`}>
              <path d={b.d} fill={b.fill} />
            </g>
          ))}
        </g>
      </svg>
    )
  }
  return (
    <svg className="agent-glyph" viewBox="0 0 24 24" width={size} height={size} fill={m.grad ? `url(#${uid})` : m.color ?? 'currentColor'} fillRule={m.even ? 'evenodd' : undefined} clipRule={m.even ? 'evenodd' : undefined} aria-hidden="true">
      {m.grad && (
        <defs>
          <linearGradient id={uid} x1="0" y1="0" x2="0" y2="1">
            {m.grad.map((c, i) => (
              <stop key={c + i} offset={m.grad!.length < 2 ? 0 : i / (m.grad!.length - 1)} stopColor={c} />
            ))}
          </linearGradient>
        </defs>
      )}
      <path d={m.d} />
    </svg>
  )
}

const OPENAI_MARK: Mark = {
  name: 'OpenAI',
  d: 'M22.282 9.821a5.985 5.985 0 00-.516-4.911 6.046 6.046 0 00-6.51-2.9A6.065 6.065 0 004.981 4.182a5.985 5.985 0 00-3.998 2.9 6.046 6.046 0 00.743 7.097 5.98 5.98 0 00.511 4.911 6.051 6.051 0 006.515 2.9A5.985 5.985 0 0013.26 24a6.056 6.056 0 005.772-4.206 5.989 5.989 0 003.998-2.9 6.056 6.056 0 00-.748-7.073zm-9.022 12.608a4.476 4.476 0 01-2.876-1.041l.142-.08 4.778-2.758a.795.795 0 00.393-.681v-6.737l2.02 1.169a.071.071 0 01.038.052v5.583a4.504 4.504 0 01-4.495 4.494zm-9.661-4.125a4.471 4.471 0 01-.535-3.014l.142.085 4.783 2.758a.771.771 0 00.781 0l5.843-3.368v2.332a.08.08 0 01-.033.062L9.74 19.95a4.499 4.499 0 01-6.141-1.646zM2.341 7.896a4.485 4.485 0 012.365-1.973V11.6a.766.766 0 00.388.677l5.814 3.354-2.02 1.169a.076.076 0 01-.071 0l-4.83-2.787a4.504 4.504 0 01-1.646-6.141zm16.596 3.856l-5.833-3.388L15.119 7.2a.076.076 0 01.071 0l4.83 2.791a4.494 4.494 0 01-.676 8.104v-5.677a.79.79 0 00-.407-.667zm2.011-3.023l-.142-.085-4.774-2.782a.776.776 0 00-.785 0L9.409 9.23V6.897a.066.066 0 01.028-.061l4.83-2.787a4.499 4.499 0 016.68 4.66zM8.307 12.863l-2.02-1.164a.08.08 0 01-.038-.057V6.074a4.499 4.499 0 017.376-3.454l-.142.081L8.704 5.459a.795.795 0 00-.393.681zm1.097-2.365l2.602-1.5 2.607 1.5v2.999l-2.597 1.5-2.607-1.5z',
}

// Which maker's mark a model belongs to, by its display label or id (e.g.
// "Gemini 4 Neon", "Claude Opus 5.5", "GPT-OSS 120B", "gpt-5.4").
export function modelMark(model?: string, desc?: string): Mark | undefined {
  const s = `${model ?? ''} ${desc ?? ''}`.toLowerCase()
  if (/\b(gemini|orchestrator|gemma)\b/.test(s)) return MARKS.gemini
  if (/\b(claude|opus|sonnet|haiku|fable)\b/.test(s)) return MARKS.claude
  if (/\b(gpt|openai|o[134](?:-mini|-pro|-preview)?|codex)\b/.test(s)) return OPENAI_MARK
  if (/\bqwen\b/.test(s)) return MARKS.qwen
  return undefined
}

// A model's maker mark beside its name, or a plain Cpu mark when the model's
// family is not one of the known ones.
export function ModelGlyph({ model, desc, size = 14, className = 'convo-model-ic' }: { model?: string; desc?: string; size?: number; className?: string }) {
  const m = modelMark(model, desc)
  const grad = useId()
  if (!m) return <Cpu size={size} className={className + ' is-fallback'} aria-hidden="true" />
  return (
    <svg className={className} viewBox="0 0 24 24" width={size} height={size} fill={m.grad ? `url(#${grad})` : m.color ?? 'currentColor'} fillRule={m.even ? 'evenodd' : undefined} clipRule={m.even ? 'evenodd' : undefined} aria-hidden="true">
      {m.grad && (
        <defs>
          <linearGradient id={grad} x1="0" y1="0" x2="0" y2="1">
            {m.grad.map((c, i) => (
              <stop key={c + i} offset={m.grad!.length < 2 ? 0 : i / (m.grad!.length - 1)} stopColor={c} />
            ))}
          </linearGradient>
        </defs>
      )}
      <path d={m.d} />
    </svg>
  )
}

// bare: the mark alone, at the size given, with no tile behind it — as the
// agents' list and the conversation's bar draw it (redesign 11-23); a tile
// only where the mark stands by itself.
export function AgentIcon({ program, size = 28, dot, bare }: { program?: string; size?: number; dot?: 'blocked' | 'working' | 'done' | 'bg' | 'failed' | 'stale'; bare?: boolean }) {
  const m = agentMark(program)
  const glyph = bare ? size : Math.round(size * 0.58)
  return (
    <span
      className={'agent-icon' + (m?.color ? '' : ' is-plain') + (bare ? ' is-bare' : '')}
      style={{ width: size, height: size, ['--agent-color' as string]: m?.color }}
      title={m?.name ?? program}
      {...(program ? { role: 'img', 'aria-label': m?.name ?? program } : { 'aria-hidden': true })}
    >
      {m ? (
        <AgentGlyph program={program!} size={glyph} />
      ) : (
        <SquareTerminal size={glyph} strokeWidth={1.75} aria-hidden="true" />
      )}
      {dot && <span className={'agent-icon-dot is-' + dot} />}
    </span>
  )
}
