-- Which status of herdr's the "done" looked at was: its terminal and herdr's
-- count of status changes then (see the gateway's statusMark). A gateway
-- started again tells by it whether a "done" is the one looked at, where the
-- agent's log cannot tell it. '' where herdr said neither.
ALTER TABLE agent_seen ADD COLUMN mark TEXT NOT NULL DEFAULT '';
