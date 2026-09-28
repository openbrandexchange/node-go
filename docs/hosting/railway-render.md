# Railway and Render

Both platforms build the included `Dockerfile` directly from your copy of this repository (fork it on GitHub, or push it to your own account).

## Render

1. In Render: **New → Blueprint**, pick your repository. `render.yaml` creates a web service with a 10 GB disk at `/data`, a generated `OBX_SECRET_KEY` and a health check.
2. After the first deploy, open **Logs** and look for the setup code; open the service URL and complete the wizard.

The public address comes from Render (`RENDER_EXTERNAL_URL`); set `OBX_PUBLIC_URL` when you add a custom domain.

## Railway

1. **New Project → Deploy from GitHub repo**, pick your repository (`railway.json` selects the Dockerfile and health check).
2. Add a **Volume** to the service with mount path `/data` (Service → Settings → Volumes), and a public domain (Settings → Networking → Generate Domain).
3. Redeploy, find the setup code in the deploy logs, and open the domain.

Railway announces the domain (`RAILWAY_PUBLIC_DOMAIN`), so `OBX_PUBLIC_URL` is set automatically. `OBX_SECRET_KEY` is generated on first start and kept in the volume; to manage it yourself, add it as a variable before the first start.

## Both

Run one instance only when you use SQLite. For PostgreSQL add the platform's database and set `OBX_DATABASE_URL`. E-mail: set `OBX_SMTP_URL` and `OBX_MAIL_FROM`.
