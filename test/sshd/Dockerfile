# Disposable SSH target for local development and integration tests.
# Two accounts (alice/bob) with password login, tmux and bash installed.
FROM alpine:3.22
RUN apk add --no-cache openssh tmux bash ncurses python3 \
 && ssh-keygen -A \
 && adduser -D -s /bin/bash alice && echo 'alice:alice-pass' | chpasswd \
 && adduser -D -s /bin/bash bob && echo 'bob:bob-pass' | chpasswd \
 && sed -i 's/^#\?PasswordAuthentication.*/PasswordAuthentication yes/; s/^#\?AllowTcpForwarding.*/AllowTcpForwarding yes/; s/^#\?AllowStreamLocalForwarding.*/AllowStreamLocalForwarding yes/' /etc/ssh/sshd_config
EXPOSE 22
CMD ["/usr/sbin/sshd", "-D", "-e"]
