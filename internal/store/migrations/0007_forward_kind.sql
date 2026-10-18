-- What a forward serves: 'port' (a TCP port on the host, port > 0) or 'code'
-- (the user's code-server on the host, port 0: one per owner and host).
ALTER TABLE forwards ADD COLUMN kind TEXT NOT NULL DEFAULT 'port';
