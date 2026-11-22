-- Add mode column ('translate' | 'summarize') to user_translate_config.
ALTER TABLE user_translate_config ADD COLUMN mode TEXT NOT NULL DEFAULT 'translate';
