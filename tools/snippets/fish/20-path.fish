# ─── PATH 装配（fish 版） ────────────────────────────────────────────────────
if command -q bs
    command bs env path --shell fish | source
end
