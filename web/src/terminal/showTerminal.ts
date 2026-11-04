import { createContext } from 'react'

// The way to the terminal of the agent shown (the conversation's and its
// cards'): undefined where there is none to go to.
export const ShowTerminal = createContext<(() => void) | undefined>(undefined)
