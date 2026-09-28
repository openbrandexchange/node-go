# Privacy notice template for your brand portal

Your brand portal processes personal data of the people at your retailers who use it. **You, the brand, are the controller**; if someone hosts the portal for you, they are your processor (make a data processing agreement with them). Below is a template you can adapt, translate and publish, for example as a page on your website that you link from the portal's welcome text. It describes what the software stores; add what you do yourself (your CRM, your e-mail provider, your hosting) and check it with your own adviser. This template is not legal advice.

Replace everything in `[square brackets]`.

---

## Privacy notice: [Brand] portal for retailers

**Who we are.** [Legal name of the brand company], [address], [chamber of commerce number] ("we") runs the portal at [https://images.yourbrand.com] for retailers who sell our products. Questions about your data: [privacy@yourbrand.com].

**What we store and why.**

| Data | Why | Legal basis (GDPR) | How long |
|---|---|---|---|
| Your name, business e-mail address, language, the company you work for | to give you an account and send you invitations, password links and "your download is ready" messages | performance of the agreement with your employer and our legitimate interest in supplying our retailers (art. 6(1)(b) and (f)) | as long as your account exists |
| Your access request (company, name, e-mail, message) | to decide on the request | our legitimate interest (art. 6(1)(f)) | [365] days after the decision |
| Sign-ins and changes you make (time, what, outcome) | security of the portal | our legitimate interest (art. 6(1)(f)) | [365] days |
| Which images and data you downloaded (time, article, number of files) | to see which retailers use which content and to improve our supply | our legitimate interest (art. 6(1)(f)) | with your name at most [400] days, per company [730] days |
| A session cookie (`obx_session`) and, in your browser, your language and selection | to keep you signed in and remember your choices | necessary for the service you asked for (no consent needed) | the session: until you sign out or [30] days; the browser data until you clear it |

We do **not** store your IP address, we do not use tracking cookies or analytics, and we do not sell or share your data for marketing.

**Who receives it.** [Our hosting provider: e.g. Google Cloud (Netherlands region) / Hetzner (Germany) / …], [our e-mail provider for the portal's messages: …], and people in our company who manage retailers. [If data leaves the EEA: which safeguards.]

**Your rights.** You can get a copy of everything the portal stores about you, as a file you can take elsewhere, and have your account deleted: ask us at [privacy@yourbrand.com]. [Only when your portal's screens offer it: you can also do both yourself in your account.] You can ask us to correct or delete your data, to restrict or object to its use, at [privacy@yourbrand.com]; we answer within one month. Deleting your account removes your name from the statistics as well. You may complain to your data protection authority (in the Netherlands: the Autoriteit Persoonsgegevens).

**Changes.** We update this notice when the portal changes what it stores; the date below shows the latest version.

[Date]

---

## How the settings map to the template

| Template | Setting (see [operations.md](operations.md#personal-data-gdpr)) |
|---|---|
| access requests [365] days | `retention.access_requests_days` (brand setting) |
| sign-ins and changes [365] days | `retention.audit_log_days` |
| downloads per company [730] days, with the name [400] | `retention.download_events_days` (the name goes after at most 400 days) |
| session [30] days | fixed in this version (30 days since the last visit) |
| "Download my data", deleting your account | `GET /admin/api/me/export`, `DELETE /admin/api/me`; for the brand `GET /admin/api/users/{id}/export`, `GET /admin/api/retailers/{id}/export` or `node-go admin personal-data` |
