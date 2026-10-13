# ─── 个人别名（示例，按需增删；改这里等价于改你自己的 rc） ──────────────────
alias ll='ls -alh'
alias gs='git status --short --branch'
alias gd='git diff'
alias gl='git log --oneline --graph --decorate -20'
command -v rg >/dev/null 2>&1 && alias grep='rg'
command -v eza >/dev/null 2>&1 && alias ls='eza'
