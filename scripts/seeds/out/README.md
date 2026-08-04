# Generated seed artifacts

This directory holds large, byte-deterministic build artifacts produced by the
seed scripts. They are **not committed to the git mirror** (size); regenerate
them locally:

```bash
# tigerbeetle_accounts.json (~1.7 MB, 5,000 accounts, all account_type=90)
python3 scripts/seeds/seed_agents.py --dry-run   # writes manifest even in dry-run
# or a full run with DATABASE_URL set
```

The artifact is deterministic for a fixed `SEED_SALT` (see `.env.example`), so
regeneration yields byte-identical content. Verified in the Wave-17 gate.
