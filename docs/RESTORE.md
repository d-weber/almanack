# Restoring Almanack from a snapshot

The whole procedure is four commands. It is short on purpose: this is the document you will
read on a bad day, possibly on a phone, possibly having not thought about this server in two
years.

**Rehearse it once before the family depends on it, and again once a year.** A restore
procedure verified in 2026 proves nothing in 2033. Rehearsing on a scratch machine costs ten
minutes; finding out here does not.

## What a snapshot is

`almanack backup` writes `almanack-YYYYMMDD-HHMMSS.db` — a complete, standalone SQLite
database, produced with `VACUUM INTO`, checked with `PRAGMA integrity_check`, fsynced and
atomically renamed. It is not an incremental, it needs no tooling to read, and it is
verified before the command exits successfully. A snapshot that exists is a snapshot that
was intact when it was written.

There are no `-wal`/`-shm` files to keep. The snapshot is self-contained.

The same command writes the snapshots in `backups/pre-migration/`, one per release that
migrated the database, and those are the ones a **rollback** restores from. The old binary
cannot open a database a newer one has migrated — it refuses any schema past what it knows
— so going back to a previous release means restoring its pre-migration snapshot as below
and then putting the old binary back. Everything entered since the upgrade goes with it.
They are never pruned, so the one you want is still there; pick it by date.

## Restoring

```sh
sudo systemctl stop almanack

# Choose the newest snapshot, or an older generation if the newest is suspect.
sudo -u almanack cp /var/lib/almanack/backups/almanack-20260727-120816.db \
                    /var/lib/almanack/almanack.db

# Anything left from the old database will be replayed over the restored one.
sudo rm -f /var/lib/almanack/almanack.db-wal /var/lib/almanack/almanack.db-shm

sudo systemctl start almanack
```

Then check, in this order:

```sh
systemctl status almanack
curl -s http://127.0.0.1:8080/healthz
```

and sign in. Your events are there, or the restore did not work — do not assume.

Deleting the `-wal` and `-shm` files is not optional. They belong to the database you just
replaced; left in place, SQLite treats them as pending changes to the restored file.

## What you lose

Everything entered between the snapshot and now. Backups run hourly by default, so that is
at most an hour of edits. Nobody is told; if the timing matters, say so in the family chat,
because someone's dentist appointment may have gone.

Sessions survive — the restored database contains them — so nobody is signed out.

Push subscriptions survive too, as long as the VAPID keys in the config are the ones the
snapshot was taken under. If you are rebuilding a machine and restore an old database with a
*new* VAPID keypair, notifications stop on every device until each person reinstalls the app.
Keep the keys with the backups.

## Rebuilding the whole machine

The snapshot alone will not do it. You also need, from off the machine:

1. `/etc/almanack/almanack.conf` — above all the **VAPID keypair**, which cannot be regenerated
   without breaking every subscription.
2. A release binary for the architecture, from
   [the releases page](https://github.com/d-weber/almanack/releases).
3. DNS and registrar access, and the router port forwards.

Then work through [install.md](install.md) as far as step 9, and instead of running
`bootstrap` in step 10, restore the snapshot as above. `bootstrap` refuses to run once any
account exists, so the order cannot be got wrong silently.

## If the snapshot itself is damaged

```sh
sqlite3 /var/lib/almanack/backups/almanack-20260727-120816.db 'PRAGMA integrity_check;'
```

Anything but `ok` means try the previous generation. Retention is generational — hourly,
daily, weekly, monthly — precisely so that corruption noticed late is still recoverable from
a version predating it.

If `almanack backup` has been exiting non-zero and mailing you about it, the newest
generations are the suspect ones. Work backwards to the last good one and expect to lose
whatever came after.

## Rehearsing

On any machine, with a copy of a snapshot:

```sh
mkdir rehearsal && cd rehearsal
cp /path/to/almanack-20260727-120816.db ./almanack.db

cat > rehearsal.conf <<'EOF'
ALMANACK_DEV=true
ALMANACK_LISTEN=127.0.0.1:8099
ALMANACK_BASE_URL=http://localhost:8099
ALMANACK_DATA=./almanack.db
ALMANACK_TZ=Europe/Paris
ALMANACK_MAIL_DIR=./mail
EOF

almanack --config rehearsal.conf serve
```

Open <http://localhost:8099>, sign in with a real account, and look at a month you remember.
That is the rehearsal: it uses dev mode so it needs no TLS and no MTA, and it touches
nothing on the real server.

Delete the directory afterwards — it contains a full copy of the family's calendar.
