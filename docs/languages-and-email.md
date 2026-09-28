# Languages and e-mail

## Interface languages

The web interface (vendored from [admin-ui](https://github.com/openbrandexchange/admin-ui) into `ui/dist`) ships 25 languages: bg, cs, da, de, el, en, es, et, fi, fr, ga, hr, hu, it, lt, lv, mt, nl, nb, pl, pt, ro, sk, sl, sv. It picks the browser language, can be switched, and remembers the choice.

Product texts are published in the brand's content languages (admin → Settings → Languages). Wherever one language is needed (e-mails, CSV fallbacks), the rule is: the reader's language, then English, then the first available language.

## E-mails

The portal sends:

| Key | When | To |
|---|---|---|
| `email.invite` | someone is invited (a retailer's buyer, also after an approved access request with portal access, or a colleague as administrator) | the invited person |
| `email.accessRequestReceived` | a retailer sent an access request | the requester |
| `email.accessRequestNew` | a retailer asks for access | every active administrator |
| `email.accessApproved` / `email.accessRejected` | a request is decided (approved without an invitation, or declined) | the requester |
| `email.zipReady` | a background download finished | the person who started it |
| `email.passwordReset` | "forgot password" | the account holder |

Texts come from the admin-ui locale files `ui/dist/locales/<lang>.json`, the same strings the interface uses. Each group holds `subject` and `body` and, where they apply, `button`, `expiry`, `ignore`, `reason`, `apiNote` and `contact`; shared are `email.greeting` (or `greetingNoName`), `email.signoff` and `email.footer`. The node sends plain-text mail in this order: greeting, body, reason, API note (only when an API key was created), button text with the link below it, expiry, ignore, contact, sign-off, footer. A line whose `{{placeholder}}` has no value is left out (for example the reason when none was given).

| Placeholder | Value |
|---|---|
| `{{portal}}` | the portal name, "<Brand> · <local word>" (for example "Kestrel · Beeldbank"); the word is the brand's own `portal_name` for that language, or `glossary.portal` from the locale file |
| `{{brand}}` | the brand's display name |
| `{{name}}` | the recipient's name |
| `{{retailer}}`, `{{contact}}` | the requesting company and person (access requests) |
| `{{reason}}` | the rejection reason, when given |
| `{{files}}` | number of files in a finished download |
| `{{expires}}` | the date a link stops working (written as 2026-10-05 in every language) |

The recipient's language is the language of their account; for access requests the language the retailer chose in the form (or their browser language). Missing texts fall back to English, and to built-in English texts in the node when the locale files do not have the key yet.

Without `OBX_SMTP_URL` nothing is sent; the admin screens then show invitation links to copy (`email_sent: false`).
