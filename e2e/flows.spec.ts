import { expect, test, type Page, type Response } from '@playwright/test'

const SETUP_CODE = process.env.OBX_E2E_SETUP_CODE ?? 'SETUP-1234'
const admin = { name: 'Iris Admin', email: 'admin@kestrel.example', password: 'kestrel-admin-2026!' }

test.describe.configure({ mode: 'serial' })

/** Collects API calls that failed unexpectedly, and page errors, so node/UI mismatches show up. */
function watch(page: Page, allow: RegExp[] = []) {
  const problems: string[] = []
  page.on('pageerror', (e) => problems.push(`pageerror: ${e.message}`))
  page.on('response', async (r: Response) => {
    const u = new URL(r.url())
    if (!u.pathname.startsWith('/admin/api/') && !u.pathname.startsWith('/obx/')) return
    if (r.status() < 400) return
    const key = `${r.request().method()} ${u.pathname} ${r.status()}`
    // A missing blob is how the UI learns it must upload an image (sha256 dedupe).
    if (/^HEAD \/admin\/api\/catalog\/blobs\/\w+ 404$/.test(key)) return
    if (allow.some((a) => a.test(key))) return
    let body = ''
    try { body = (await r.text()).slice(0, 300) } catch {}
    problems.push(`${key} ${body}`)
  })
  return problems
}

// Every page runs under the node's real Content-Security-Policy; a violation fails the test,
// so a node that tightens (or loosens) its policy against what admin-ui needs is caught here.
const cspViolations: string[] = []
test.beforeEach(async ({ page }) => {
  cspViolations.length = 0
  page.on('console', (m) => {
    if (m.type() === 'error' && /Content Security Policy/i.test(m.text())) cspViolations.push(m.text())
  })
  await page.addInitScript(() => localStorage.setItem('obx.lang', 'en'))
})
test.afterEach(() => {
  expect(cspViolations).toEqual([])
})

test('first-run wizard creates the brand and the admin account', async ({ page }) => {
  const problems = watch(page)
  await page.goto('/')
  await expect(page.getByRole('heading', { name: 'Set up your brand portal' })).toBeVisible()
  await page.getByLabel('Setup code').fill(SETUP_CODE)
  await page.getByRole('button', { name: 'Next' }).click()
  await page.getByLabel('Company name').fill('Kestrel Outfitters')
  await page.getByLabel('First brand').fill('Kestrel')
  await page.getByRole('button', { name: 'Next' }).click()
  await page.getByRole('checkbox', { name: 'Nederlands' }).check()
  await page.getByRole('button', { name: 'Next' }).click()
  await page.getByLabel('Your name').fill(admin.name)
  await page.getByLabel('Email address').fill(admin.email)
  await page.getByLabel('Password', { exact: true }).fill(admin.password)
  await page.getByLabel('Repeat the password').fill(admin.password)
  await page.getByRole('button', { name: 'Finish and log in' }).click()
  await expect(page).toHaveURL(/\/admin/)
  await expect(page.getByRole('heading', { level: 1 })).toBeVisible()
  await page.screenshot({ path: 'screenshots/01-dashboard.png' })
  expect(problems).toEqual([])
})

const FIX = process.env.OBX_E2E_FIXTURES ?? '../testdata/spec/kestrel'
const IMAGES_ZIP = process.env.OBX_E2E_IMAGES_ZIP ?? 'tmp/images.zip'
const NODE_LOG = process.env.OBX_E2E_NODE_LOG ?? 'tmp/node.log'
const retailer = { shop: 'Harbour Street Boutique', name: 'Sam Buyer', email: 'buyer@harbour.example', password: 'harbour-shop-2026!' }

async function logIn(page: Page, who: { email: string; password: string }) {
  await page.goto('/login')
  await page.getByLabel('Email address').fill(who.email)
  await page.getByLabel('Password', { exact: true }).fill(who.password)
  await page.getByRole('button', { name: 'Log in' }).click()
}

test('a CSV plus a zip of images imports the whole catalog', async ({ page }) => {
  const problems = watch(page)
  await logIn(page, admin)
  await page.getByRole('link', { name: 'Imports' }).first().click()
  await page.getByTestId('import-file-input').setInputFiles([`${FIX}/products.csv`, `${FIX}/assets.csv`, IMAGES_ZIP])
  await expect(page.getByText('27 images')).toBeVisible()
  await page.getByRole('button', { name: 'Start import' }).click()
  await expect(page).toHaveURL(/\/admin\/imports\/.+/)
  await expect(page.getByText('Everything was imported without problems.')).toBeVisible({ timeout: 90_000 })
  await page.screenshot({ path: 'screenshots/02-import.png', fullPage: true })
  await page.getByRole('link', { name: 'Catalog' }).first().click()
  await expect(page.getByText('KO-1001').first()).toBeVisible()
  expect(problems).toEqual([])
})

test('a retailer requests access and the brand approves it', async ({ page }) => {
  const problems = watch(page)
  await page.goto('/request-access')
  await page.getByLabel('Shop or company name').fill(retailer.shop)
  await page.getByLabel('Your name').fill(retailer.name)
  await page.getByLabel('Email address').fill(retailer.email)
  await page.getByRole('button', { name: 'Send request' }).click()
  await expect(page.getByRole('heading', { name: 'Request sent' })).toBeVisible()
  await page.screenshot({ path: 'screenshots/03-request-sent.png' })

  await logIn(page, admin)
  await page.getByRole('link', { name: 'Access requests' }).first().click()
  await expect(page.getByText(retailer.shop).first()).toBeVisible()
  await page.getByRole('button', { name: 'Approve' }).first().click()
  const dialog = page.getByRole('dialog')
  await expect(dialog).toBeVisible()
  await page.screenshot({ path: 'screenshots/04-approve.png' })
  await dialog.getByRole('button', { name: 'Approve' }).click()
  await expect(page.getByText(`${retailer.shop} is approved.`)).toBeVisible()
  expect(problems).toEqual([])
})

async function inviteLink(email: string): Promise<string> {
  const { readFile } = await import('node:fs/promises')
  for (let i = 0; i < 40; i++) {
    const log = await readFile(NODE_LOG, 'utf8')
    const lines = log.split('\n').filter((l) => l.includes(email) && l.includes('/invite/'))
    const m = lines.at(-1)?.match(/https?:\/\/[^\s"\\]+\/invite\/[A-Za-z0-9_\-.~%]+/)
    if (m) return m[0]
    await new Promise((r) => setTimeout(r, 250))
  }
  throw new Error(`no invite for ${email} in ${NODE_LOG}`)
}

test('the invited retailer sets a password, browses, pastes EANs and downloads a zip', async ({ page }) => {
  const problems = watch(page)
  const link = await inviteLink(retailer.email)
  await page.goto(new URL(link).pathname)
  await expect(page.getByRole('heading', { name: 'Welcome' })).toBeVisible()
  await page.getByLabel('New password').fill(retailer.password)
  await page.getByLabel('Repeat the password').fill(retailer.password)
  await page.getByRole('button', { name: 'Create my account' }).click()

  const list = page.getByRole('list', { name: 'Articles' })
  await expect(list.getByRole('listitem').first()).toBeVisible()
  await expect(page.getByText('8 articles')).toBeVisible()
  await page.screenshot({ path: 'screenshots/05-portal.png' })

  await list.getByRole('listitem').first().getByRole('link').first().click()
  await expect(page.getByRole('heading', { name: 'Sizes and EANs' })).toBeVisible()
  await page.screenshot({ path: 'screenshots/06-product.png' })

  await page.getByRole('link', { name: 'EAN list' }).first().click()
  await page.getByLabel('EANs', { exact: true }).fill('2000100101018\n2000100101025\n2000100201015\n9999999999994\nabc')
  await page.getByRole('button', { name: 'Find articles' }).click()
  await expect(page.getByText(/found/).first()).toBeVisible()
  await expect(page.getByText('1 not found')).toBeVisible()
  await page.screenshot({ path: 'screenshots/07-ean.png', fullPage: true })
  await page.getByRole('button', { name: /Add \d+ articles? to selection/ }).click()
  await page.getByRole('link', { name: 'Selection' }).first().click()
  const download = page.waitForEvent('download')
  await page.getByRole('button', { name: /Download \d+ images \(zip\)/ }).click()
  const zip = await download
  expect(zip.suggestedFilename()).toMatch(/\.zip$/)
  await page.screenshot({ path: 'screenshots/08-selection.png', fullPage: true })
  expect(problems).toEqual([])
})

test('the brand creates an API key for a retailer and allows self-service keys', async ({ page }) => {
  const problems = watch(page)
  await logIn(page, admin)
  await page.getByRole('link', { name: 'Retailers' }).first().click()
  await page.getByRole('link', { name: retailer.shop }).first().click()
  await page.getByLabel('Name of the key').fill('Harbour PIM')
  await page.getByRole('button', { name: 'Create API key' }).click()
  await expect(page.getByText('Client secret')).toBeVisible()
  await page.screenshot({ path: 'screenshots/09-admin-api-key.png', fullPage: true })

  await page.getByRole('link', { name: 'Settings' }).first().click()
  await page.getByRole('tab', { name: 'Portal' }).click()
  const selfService = page.getByRole('switch', { name: 'Retailers may create their own API keys' })
  await selfService.click()
  await expect(selfService).toHaveAttribute('aria-checked', 'true')
  expect(problems).toEqual([])
})

test('an invitation to a shop in Germany is written in German', async ({ page }) => {
  const problems = watch(page)
  await logIn(page, admin)
  await page.getByRole('link', { name: 'Retailers' }).first().click()
  await page.getByRole('link', { name: 'Add retailer' }).first().click()
  await page.getByLabel('Shop or company name').fill('Modehaus Adler')
  await page.getByLabel('Country').selectOption('DE')
  await page.getByRole('button', { name: 'Add retailer' }).click()
  // The language of the e-mail follows the shop's country; the brand admin works in English.
  await expect(page.getByLabel('Language of the email')).toHaveValue('de')
  await page.getByLabel('Email address').fill('jana@adler.example')
  await page.getByRole('button', { name: 'Invite', exact: true }).click()
  await expect(page.getByText(/Send this link to jana@adler.example yourself/)).toBeVisible()
  const { readFile } = await import('node:fs/promises')
  await expect.poll(async () => (await readFile(NODE_LOG, 'utf8')).split('\n').find((l) => l.includes('jana@adler.example') && l.includes('lädt Sie')) ?? '').not.toBe('')
  expect(problems).toEqual([])
})

test('the retailer creates an API key for their software', async ({ page }) => {
  const problems = watch(page)
  await logIn(page, retailer)
  await page.getByRole('link', { name: 'Account' }).first().click()
  await page.getByLabel('Name of the key').fill('Shop system')
  await page.getByRole('button', { name: 'Create API key' }).click()
  await expect(page.getByText('Client secret')).toBeVisible()
  await page.screenshot({ path: 'screenshots/10-retailer-api-key.png', fullPage: true })
  expect(problems).toEqual([])
})

test('the retailer downloads a product zip, the article data and a whole-catalog zip prepared in the background', async ({ page }) => {
  const problems = watch(page)
  await logIn(page, retailer)
  const list = page.getByRole('list', { name: 'Articles' })
  await list.getByRole('listitem').first().getByRole('link').first().click()
  let download = page.waitForEvent('download')
  await page.getByRole('button', { name: /Download all \d+ images/ }).click()
  expect((await download).suggestedFilename()).toMatch(/\.zip$/)

  await page.getByRole('link', { name: 'Catalog' }).first().click()
  download = page.waitForEvent('download')
  await page.getByRole('button', { name: 'Article data (CSV)' }).click()
  const csv = await download
  expect(csv.suggestedFilename()).toMatch(/\.csv$/)
  const { readFile } = await import('node:fs/promises')
  expect(await readFile((await csv.path())!, 'utf8')).toContain('2000100101018')

  await page.getByRole('button', { name: 'Download all' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Prepare zip' }).click()
  await expect(page.getByText(/We're preparing your zip/)).toBeVisible()
  await page.getByRole('dialog').getByRole('link', { name: 'Go to Downloads' }).click()
  await expect(page.getByText('Ready').first()).toBeVisible({ timeout: 60_000 })
  download = page.waitForEvent('download')
  await page.getByRole('main').getByRole('button', { name: 'Download', exact: true }).first().click()
  expect((await download).suggestedFilename()).toMatch(/\.zip$/)
  await page.screenshot({ path: 'screenshots/11-downloads.png', fullPage: true })
  expect(problems).toEqual([])
})

test('the brand creates an access key for automatic upload', async ({ page }) => {
  const problems = watch(page)
  await logIn(page, admin)
  await page.getByRole('link', { name: 'Automatic upload' }).first().click()
  await page.getByRole('tab', { name: 'Access keys' }).click()
  await page.getByLabel('Name', { exact: true }).fill('PIM nightly export')
  await page.getByRole('button', { name: 'Create access key' }).click()
  await expect(page.getByText('Access key for PIM nightly export')).toBeVisible()
  await expect(page.getByText(/obx_it_|obxi_|obx_/).first()).toBeVisible()
  await page.screenshot({ path: 'screenshots/12-access-key.png', fullPage: true })
  expect(problems).toEqual([])
})

test('a scheduled import fetches the nightly export from a web address', async ({ page }) => {
  const problems = watch(page)
  const { createServer } = await import('node:http')
  const { readFile } = await import('node:fs/promises')
  const pkg = await readFile(process.env.OBX_E2E_PACKAGE_ZIP ?? 'tmp/package.zip')
  const server = createServer((req, res) => {
    if (req.url !== '/exports/kestrel.zip') return void res.writeHead(404).end()
    res.writeHead(200, { 'Content-Type': 'application/zip', 'Content-Length': pkg.length }).end(pkg)
  })
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', () => r()))
  const port = (server.address() as { port: number }).port
  try {
    await logIn(page, admin)
    await page.getByRole('link', { name: 'Automatic upload' }).first().click()
    await page.getByRole('link', { name: 'Add scheduled import' }).or(page.getByRole('button', { name: 'Add scheduled import' })).first().click()
    await page.getByLabel('Name', { exact: true }).fill('Nightly PIM export')
    await page.getByLabel('Web address of the CSV or zip').fill(`http://127.0.0.1:${port}/exports/kestrel.zip`)
    await page.getByLabel('How often').selectOption('daily')
    await page.getByRole('button', { name: 'Save' }).click()
    await expect(page.getByText('Nightly PIM export')).toBeVisible()
    await page.getByRole('button', { name: 'Test connection' }).click()
    await expect(page.getByText(/Connected\./)).toBeVisible()
    await page.getByRole('button', { name: 'Run now' }).click()
    await page.getByRole('link', { name: 'Follow it' }).click()
    await expect(page.getByText(/1 updated|Updated/).first()).toBeVisible({ timeout: 60_000 })
    await expect(page.getByText('Everything was imported without problems.')).toBeVisible({ timeout: 60_000 })
    await page.screenshot({ path: 'screenshots/13-scheduled-import.png', fullPage: true })
  } finally {
    server.close()
  }
  expect(problems).toEqual([])
})

async function expectImagesLoaded(page: Page) {
  await expect.poll(async () =>
    page.locator('main img').evaluateAll((imgs) => imgs.length > 0 && imgs.every((i) => (i as HTMLImageElement).complete && (i as HTMLImageElement).naturalWidth > 0)),
  ).toBe(true)
}

test('the brand opens an article in the admin and sees its images', async ({ page }) => {
  const problems = watch(page)
  await logIn(page, admin)
  await page.getByRole('link', { name: 'Catalog' }).first().click()
  await page.getByRole('link', { name: /KO-1001/ }).first().click()
  await expect(page.getByRole('heading', { level: 1 })).toContainText(/Merino/)
  await expectImagesLoaded(page)
  await page.screenshot({ path: 'screenshots/14-admin-article.png', fullPage: true })
  expect(problems).toEqual([])
})

test('a Dutch retailer sees the portal as a Beeldbank', async ({ page }) => {
  const problems = watch(page)
  await logIn(page, retailer)
  await page.getByTestId('language-switcher').first().selectOption('nl')
  await expect(page.getByText('Beeldbank').first()).toBeVisible()
  await expect(page.getByText('8 artikelen')).toBeVisible()
  await expectImagesLoaded(page)
  await page.screenshot({ path: 'screenshots/15-portal-nl.png' })
  expect(problems).toEqual([])
})

test('the example zip from the README imports on its own', async ({ page }) => {
  const problems = watch(page)
  await logIn(page, admin)
  await page.getByRole('link', { name: 'Imports' }).first().click()
  await page.getByTestId('import-file-input').setInputFiles(['../testdata/kestrel-example.zip'])
  await expect(page.getByText('27 images')).toBeVisible()
  await page.getByRole('button', { name: 'Start import' }).click()
  await expect(page.getByText('Everything was imported without problems.')).toBeVisible({ timeout: 90_000 })
  expect(problems).toEqual([])
})
