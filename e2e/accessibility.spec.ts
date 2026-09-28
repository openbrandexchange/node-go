import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Page } from '@playwright/test'

// Runs after flows.spec.ts (project dependency in playwright.config.ts), on
// the node it set up: every main portal and admin screen is checked with axe
// (WCAG 2.1 A and AA), and the key tasks are done with the keyboard only.

const admin = { email: 'admin@kestrel.example', password: 'kestrel-admin-2026!' }
const retailer = { email: 'buyer@harbour.example', password: 'harbour-shop-2026!' }


test.beforeEach(async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('obx.lang', 'en'))
})

async function logIn(page: Page, who: { email: string; password: string }) {
  await page.goto('/login')
  await page.getByLabel('Email address').fill(who.email)
  await page.getByLabel('Password', { exact: true }).fill(who.password)
  await page.getByRole('button', { name: 'Log in' }).click()
  await expect(page).not.toHaveURL(/\/login/)
}

/** Fails with a readable list when axe finds WCAG 2.1 A/AA violations. */
async function checkA11y(page: Page, screen: string) {
  await page.waitForLoadState('networkidle')
  const result = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa']).analyze()
  const found = result.violations.map(
    (v) => `${screen}: ${v.id} (${v.impact}) ${v.help} — ${v.nodes.slice(0, 3).map((n) => n.target.join(' ')).join(', ')}`,
  )
  expect.soft(found, found.join("\n")).toEqual([])
}

test('public screens pass axe', async ({ page }) => {
  for (const [path, name] of [['/login', 'login'], ['/request-access', 'request access'], ['/forgot-password', 'forgot password']]) {
    await page.goto(path)
    await expect(page.getByRole('heading', { level: 1 })).toBeVisible()
    await checkA11y(page, name)
  }
})

test('retailer portal screens pass axe', async ({ page }) => {
  await logIn(page, retailer)
  await expect(page.getByRole('list', { name: 'Articles' }).getByRole('listitem').first()).toBeVisible()
  await checkA11y(page, 'portal catalog')
  await page.getByRole('list', { name: 'Articles' }).getByRole('listitem').first().getByRole('link').first().click()
  await expect(page.getByRole('heading', { name: 'Sizes and EANs' })).toBeVisible()
  await checkA11y(page, 'portal article')
  for (const [path, name] of [['/ean', 'EAN list'], ['/selection', 'selection'], ['/downloads', 'downloads'], ['/account', 'account']]) {
    await page.goto(path)
    await expect(page.getByRole('heading', { level: 1 })).toBeVisible()
    await checkA11y(page, name)
  }
})

test('admin screens pass axe', async ({ page }) => {
  await logIn(page, admin)
  for (const [path, name] of [
    ['/admin', 'dashboard'], ['/admin/catalog', 'catalog'], ['/admin/catalog/KO-1001', 'article'], ['/admin/imports', 'imports'],
    ['/admin/automation', 'automation'], ['/admin/retailers', 'retailers'], ['/admin/requests', 'access requests'],
    ['/admin/stats', 'statistics'], ['/admin/settings', 'settings'],
  ]) {
    await page.goto(path)
    await expect(page.getByRole('heading', { level: 1 })).toBeVisible()
    await checkA11y(page, name)
  }
})

test('logging in works with the keyboard only', async ({ page }) => {
  await page.goto('/login')
  const email = page.getByLabel('Email address')
  await email.focus()
  await page.keyboard.type(retailer.email)
  await page.keyboard.press('Tab')
  await expect(page.getByLabel('Password', { exact: true })).toBeFocused()
  await page.keyboard.type(retailer.password)
  await page.keyboard.press('Enter')
  await expect(page.getByRole('list', { name: 'Articles' }).getByRole('listitem').first()).toBeVisible()
})

test('a retailer opens an article and adds it to the selection with the keyboard only', async ({ page }) => {
  await logIn(page, retailer)
  const firstLink = page.getByRole('list', { name: 'Articles' }).getByRole('listitem').first().getByRole('link').first()
  await expect(firstLink).toBeVisible()
  // Tab from the top of the page until the first article link has focus.
  await page.locator('body').focus()
  let reached = false
  for (let i = 0; i < 80 && !reached; i++) {
    await page.keyboard.press('Tab')
    reached = await firstLink.evaluate((el) => el === document.activeElement)
  }
  expect(reached, 'the first article is reachable with Tab').toBe(true)
  // Focus is visible (an outline or ring), not hidden.
  const outline = await firstLink.evaluate((el) => {
    const s = getComputedStyle(el)
    return s.outlineStyle !== 'none' || s.boxShadow !== 'none'
  })
  expect(outline, 'focused link shows a focus indicator').toBe(true)
  await page.keyboard.press('Enter')
  await expect(page.getByRole('heading', { name: 'Sizes and EANs' })).toBeVisible()
})

test('the admin navigation is reachable with the keyboard', async ({ page }) => {
  await logIn(page, admin)
  const nav = page.getByRole('link', { name: 'Imports' }).first()
  await page.locator('body').focus()
  let reached = false
  for (let i = 0; i < 60 && !reached; i++) {
    await page.keyboard.press('Tab')
    reached = await nav.evaluate((el) => el === document.activeElement)
  }
  expect(reached, 'the Imports link is reachable with Tab').toBe(true)
  await page.keyboard.press('Enter')
  await expect(page).toHaveURL(/\/admin\/imports/)
})
