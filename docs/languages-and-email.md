# Languages and e-mail

## Interface languages

The web interface (vendored from [admin-ui](https://github.com/openbrandexchange/admin-ui) into `ui/dist`) ships 25 languages: bg, cs, da, de, el, en, es, et, fi, fr, ga, hr, hu, it, lt, lv, mt, nl, nb, pl, pt, ro, sk, sl, sv. It picks the browser language, can be switched, and remembers the choice.

Product texts are published in the brand's content languages (admin → Settings → Languages). Wherever one language is needed (e-mails, CSV fallbacks), the rule is: the reader's language, then English, then the first available language.

## E-mails

The portal sends:

| Key | When | To |
|---|---|---|
| `email.invite` | a retailer user is invited (also after an approved access request with portal access) | the invited person |
| `email.invite_admin` | a colleague is invited as administrator | the invited person |
| `email.access_request` | a retailer asks for access | every active administrator |
| `email.access_approved` / `email.access_rejected` | a request is decided | the requester |
| `email.zip_ready` | a background download finished | the person who started it |
| `email.password_reset` | "forgot password" | the account holder |

Texts come from the admin-ui locale files: `ui/dist/locales/<lang>.json`, keys `email.<kind>.subject` and `email.<kind>.body`, plus `email.greeting` and `email.footer`. Keys may be nested (`{"email": {"invite": {"subject": …}}}`) or flat. Placeholders are written `{name}` or `{{name}}`:

| Placeholder | Value |
|---|---|
| `{portal}` | the portal name, "<Brand> · <local word>" (for example "Kestrel · Beeldbank"), or the brand's own name for it |
| `{brand}` | the brand's display name |
| `{name}` | the recipient's name |
| `{link}` | the one-time or target link |
| `{retailer}`, `{contact}` | the requesting company and person (access requests) |
| `{reason}` | the rejection reason, when given |
| `{files}`, `{expires_days}` | download size and how long a link stays valid |

The recipient's language is the language of their account; for access requests the language the retailer chose in the form (or their browser language). Missing texts fall back to English, and to built-in English texts in the node when the locale files do not have the key yet.

Without `OBX_SMTP_URL` nothing is sent; the admin screens then show invitation links to copy (`email_sent: false`).
