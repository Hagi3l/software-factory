# Vault demo — VPS deploy runbook

One-time setup for the DigitalOcean droplet the vault demo deploys to. Do this **once**,
then **snapshot the droplet** — on demo day you restore the snapshot, re-attach the Reserved
IP, and everything below is already in place.

This file is harness-side (private) on purpose: it describes *your* VPS, domain, and
hardening. It is **not** part of the public vault repo — that repo only ever contains the app
and the features the agents add.

Target: `https://vault.lochie.dev`, deployed by `app/.github/workflows/deploy.yml` on every
push to the public repo's `main`.

```
browser ──HTTPS(443)──> Caddy ──> vault (127.0.0.1:8000)
GitHub Action ──SSH(22)──> droplet
vault.lochie.dev ──DNS──> DO Reserved IP ──> droplet (snapshot-restored)
```

## 1. Reserved IP + DNS (stable address)

1. **DigitalOcean → Networking → Reserved IPs** — create one, assign it to the droplet. This
   IP is stable across snapshot restores (just re-assign it), so DNS never needs updating.
2. **DNS for `lochie.dev`** — add an `A` record: name `vault`, value = the Reserved IP,
   **proxy off (grey cloud / DNS-only)**, TTL 60s. Grey cloud is required — SSH and Caddy's
   HTTP-01 cert challenge both need the real IP reachable.
3. Verify: `dig +short vault.lochie.dev` returns the Reserved IP.

## 2. Harden SSH

In `/etc/ssh/sshd_config`:

```
PasswordAuthentication no
PermitRootLogin no
```

Then `sudo systemctl restart ssh`. Open the firewall for the three ports we use:

```bash
sudo ufw allow 22,80,443/tcp && sudo ufw enable
```

On a 512MB droplet, add 1GB of swap so a momentary spike (an `apt install`, a snapshot
restore) can't OOM-kill anything — nothing *builds* here (the binary is compiled in CI), so
this is just insurance:

```bash
sudo fallocate -l 1G /swapfile && sudo chmod 600 /swapfile
sudo mkswap /swapfile && sudo swapon /swapfile
echo '/swapfile none swap sw 0 0' | sudo tee -a /etc/fstab
```

## 3. Deploy user (what GitHub Actions logs in as)

```bash
sudo adduser --disabled-password --gecos "" deploy
sudo mkdir -p /home/deploy/.ssh && sudo chmod 700 /home/deploy/.ssh
# Put the PUBLIC half of the Actions keypair here:
echo 'ssh-ed25519 AAAA... actions-deploy' | sudo tee /home/deploy/.ssh/authorized_keys
sudo chown -R deploy:deploy /home/deploy/.ssh && sudo chmod 600 /home/deploy/.ssh/authorized_keys
```

Generate the keypair locally with `ssh-keygen -t ed25519 -f vault-deploy -C actions-deploy`;
the **private** half becomes the `VPS_SSH_KEY` GitHub secret (step 6).

Narrow its sudo to *exactly* the two commands the workflow runs — so even a leaked key can
only redeploy the vault, not get root. In `/etc/sudoers.d/deploy` (`sudo visudo -f
/etc/sudoers.d/deploy`):

```
deploy ALL=(root) NOPASSWD: /usr/bin/install -m 0755 /tmp/vault-deploy/vault /usr/local/bin/vault, /usr/bin/systemctl restart vault
```

(Verify the paths: `which install systemctl` — adjust if they differ on your image.)

## 4. The vault service (bound to loopback, behind Caddy)

A dedicated unprivileged user owns the data; the binary listens only on loopback (Caddy is
the only thing facing the internet):

```bash
sudo adduser --system --group --no-create-home vault
sudo mkdir -p /var/lib/vault && sudo chown vault:vault /var/lib/vault
```

`/etc/systemd/system/vault.service`:

```ini
[Unit]
Description=Secrets vault demo
After=network.target

[Service]
User=vault
Group=vault
Environment=VAULT_ADDR=127.0.0.1:8000
Environment=VAULT_DB=/var/lib/vault/vault.db
WorkingDirectory=/var/lib/vault
ExecStart=/usr/local/bin/vault
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

The first deploy installs `/usr/local/bin/vault`; until then `systemctl enable --now vault`
will fail to start (no binary yet) — that's fine, the workflow's `restart` brings it up after
the binary lands. Run `sudo systemctl enable vault` now so it autostarts on reboot/restore.

## 5. Caddy (auto-HTTPS)

Stock Caddy — no plugins, no custom build (single host, port 80 open, DNS-only ⇒ the default
HTTP-01 challenge works):

```bash
sudo apt install -y caddy   # or the official Caddy apt repo
```

`/etc/caddy/Caddyfile`:

```
vault.lochie.dev {
    reverse_proxy 127.0.0.1:8000
}
```

`sudo systemctl reload caddy`. Caddy fetches a Let's Encrypt cert for `vault.lochie.dev`
automatically on first request.

## 6. GitHub Actions secrets

On the public vault repo → Settings → Secrets and variables → Actions:

| Secret | Value |
|---|---|
| `VPS_HOST` | `vault.lochie.dev` (or the Reserved IP) |
| `VPS_USER` | `deploy` |
| `VPS_SSH_KEY` | the **private** half of the `vault-deploy` keypair |

## 7. Snapshot, then demo day

1. With everything above in place, **power off and snapshot** the droplet
   (DigitalOcean → droplet → Snapshots).
2. Destroy the droplet to stop paying for it.
3. **Demo day:** create a droplet from the snapshot, re-attach the Reserved IP, confirm
   `vault.lochie.dev` resolves, and you're live. Nothing else changes — `VPS_HOST` and the
   browser URL are stable. Destroy it again afterward.
