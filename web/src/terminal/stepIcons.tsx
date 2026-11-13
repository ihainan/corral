// The mark before each step of a turn's work, and before the line a run of
// steps is folded to: what kind of thing was done, seen before it is read.
// Lucide's, as the rest of the page; a quiet grey (terminal.css,
// .convo-step-ic). The words stay beside it: a mark is read faster, not
// instead.
import {
  Activity,
  AppWindow,
  Bot,
  Brain,
  CalendarClock,
  CircleStop,
  Eye,
  FilePenLine,
  FileText,
  Globe,
  Hourglass,
  Keyboard,
  Lightbulb,
  ListTodo,
  MessageCircleQuestionMark,
  MousePointerClick,
  Plug,
  Search,
  Send,
  Sparkles,
  SquarePen,
  Terminal,
  Users,
  Wrench,
  type LucideIcon,
} from 'lucide-react'
import { verbOf, type Category, type Verb } from './convoItems'
import type { ConvoTool } from './conversation'

// One a verb, none twice: a verb without its own does not compile.
const VERB_ICON: Record<Verb, LucideIcon> = {
  cmd: Terminal,
  read: Eye,
  edit: SquarePen,
  write: FilePenLine, // (a file written whole: new, or over what was there)
  search: Search,
  web: Globe,
  agent: Bot,
  task: ListTodo,
  ask: MessageCircleQuestionMark,
  wait: Hourglass,
  stop: CircleStop,
  page: AppWindow,
  send: Send,
  skill: Sparkles,
  agents: Users,
  cron: CalendarClock,
  advisor: Lightbulb,
  browser: MousePointerClick,
  doc: FileText,
  mcp: Plug,
  other: Wrench,
}

// The kinds a run's line counts, as its words name them first.
const CATEGORY_ICON: Record<Category, LucideIcon> = {
  cmd: Terminal,
  read: Eye,
  edit: SquarePen,
  search: Search,
  web: Globe,
  agent: Bot,
  other: Wrench,
}

export const ThinkIcon = Brain
export const WorkIcon = Activity

// A call's mark. A search of the web is the web's (its verb is a search of
// the code's: they are counted together); what was typed into a command
// still running, a keyboard.
export function stepIcon(tool: ConvoTool): LucideIcon {
  if (tool.stdin) return Keyboard
  if (tool.name === 'WebSearch') return Globe
  const v = verbOf(tool.name)
  return v ? VERB_ICON[v] : Wrench
}

export const categoryIcon = (c: Category): LucideIcon => CATEGORY_ICON[c]
