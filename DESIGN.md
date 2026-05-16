---
name: google-maps-scraper-lite
description: Dense, calm Web UI for controlling local scraper jobs.
colors:
  app-bg: "#f6f8fb"
  surface: "#ffffff"
  surface-muted: "#eef2f7"
  border: "#d8e0ea"
  border-strong: "#c4cfdd"
  text: "#0f172a"
  muted: "#475569"
  muted-subtle: "#64748b"
  primary: "#1e40af"
  primary-hover: "#1d4ed8"
  primary-soft: "#eaf1ff"
  info: "#2563eb"
  danger: "#b42318"
  danger-soft: "#fef3f2"
  success: "#027a48"
  success-soft: "#ecfdf3"
  warning: "#b45309"
  warning-soft: "#fffbeb"
typography:
  headline:
    fontFamily: "Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, Segoe UI, sans-serif"
    fontSize: "1.24rem"
    fontWeight: 850
    lineHeight: 1.2
    letterSpacing: "0"
  title:
    fontFamily: "Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, Segoe UI, sans-serif"
    fontSize: "0.98rem"
    fontWeight: 850
    lineHeight: 1.2
    letterSpacing: "0"
  body:
    fontFamily: "Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, Segoe UI, sans-serif"
    fontSize: "14px"
    fontWeight: 400
    lineHeight: 1.42
    letterSpacing: "0"
  label:
    fontFamily: "Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, Segoe UI, sans-serif"
    fontSize: "0.82rem"
    fontWeight: 700
    lineHeight: 1.42
    letterSpacing: "0"
  mono:
    fontFamily: "SFMono-Regular, Consolas, Liberation Mono, monospace"
    fontSize: "0.82rem"
    lineHeight: 1.5
rounded:
  sm: "5px"
  md: "6px"
  lg: "8px"
  pill: "999px"
spacing:
  xs: "4px"
  sm: "7px"
  md: "10px"
  lg: "14px"
  xl: "24px"
components:
  button-primary:
    backgroundColor: "{colors.primary}"
    textColor: "{colors.surface}"
    rounded: "{rounded.md}"
    padding: "6px 10px"
    height: "38px"
  button-secondary:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.text}"
    rounded: "{rounded.md}"
    padding: "6px 10px"
    height: "32px"
  input-field:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.text}"
    rounded: "{rounded.md}"
    padding: "8px 9px"
  panel:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.text}"
    rounded: "{rounded.lg}"
    padding: "14px"
  status-chip:
    backgroundColor: "{colors.primary-soft}"
    textColor: "{colors.primary}"
    rounded: "{rounded.sm}"
    padding: "3px 8px"
---

# Design System: google-maps-scraper-lite

## 1. Overview

**Creative North Star: "The Operator Console"**

This interface is a local control surface for developers running scraper jobs. It should feel like a precise console for live work: dense enough to show queue state, progress, job metadata, and safe actions, but calm enough that the next move is obvious.

The system uses light structural depth everywhere: pale layered surfaces, firm borders, compact spacing, and small shadows that separate working areas without making the UI feel like a platform. Visual emphasis belongs to job state, current navigation, primary actions, and failure conditions.

It must preserve the product direction from `PRODUCT.md`: simple, professional, informative, and "less is more." It explicitly rejects complicated platform patterns, enterprise BI density, hacker-terminal styling, generic admin-template sprawl, and analytics-first presentation.

**Key Characteristics:**
- Compact operational density with stable grids and predictable controls.
- Restrained blue primary color used for selected navigation, primary actions, and pending/paused states.
- Semantic status colors for running, blocked, failed, and warning states.
- System-style typography with heavy labels and tabular numbers for scan speed.
- Mobile-responsive viewing through structural grid collapse, not fluid type.

## 2. Colors

The palette is a restrained operational blue system over cool tinted neutrals, with semantic status colors reserved for job lifecycle meaning.

### Primary
- **Operator Blue**: Primary actions, selected navigation, active filters, and pending or paused status states. Use sparingly so it remains an action and selection signal.
- **Operator Blue Hover**: Hover state for primary actions. It is close to the primary, not a decorative second accent.
- **Soft Operator Blue**: Selected rows, pending states, bulk language selections, and subtle active backgrounds.
- **Information Blue**: Informational state color. Keep separate from primary action color when the meaning is not direct action.

### Neutral
- **Workspace Mist**: App background. It gives the Web UI a quiet working canvas.
- **Surface White**: Panels, inputs, dialogs, table bodies, and most interactive controls.
- **Muted Surface**: Secondary surface fill for badges, index markers, and low-emphasis regions.
- **Cool Border**: Default panel, table, and component border.
- **Strong Cool Border**: Form input border and stronger separators.
- **Console Ink**: Primary text. Use for headings, item titles, and important data.
- **Muted Slate**: Secondary metadata, helper copy, subtitles, and inactive navigation.
- **Subtle Slate**: Tertiary metadata and compact chip labels.

### Tertiary
- **Success Green**: Running and completed progress. Use for actual success or active healthy state only.
- **Soft Success Green**: Running, done, and optional requirement badge fills.
- **Warning Amber**: Blocked, pausing, and caution states.
- **Soft Warning Amber**: Warning note and blocked status backgrounds.
- **Failure Red**: Delete actions, failed states, validation errors, and destructive emphasis.
- **Soft Failure Red**: Failed status and required badge backgrounds.

### Named Rules

**The Action Color Rule.** Operator Blue is for current location, primary action, and selected state. Do not use it as decoration.

**The Status Means State Rule.** Green, amber, and red must map to real job or form state. Do not use semantic colors for visual variety.

## 3. Typography

**Display Font:** Inter/system UI stack with platform sans fallbacks.
**Body Font:** Inter/system UI stack with platform sans fallbacks.
**Label/Mono Font:** SFMono-Regular, Consolas, Liberation Mono, monospace for code, IDs, JSON, and logs.

**Character:** The type system is compact, technical, and readable. Heavy weights create hierarchy inside dense panels; monospace is reserved for machine-readable values.

### Hierarchy
- **Display**: Not used. This product UI has no hero type.
- **Headline** (850, 1.24rem, 1.2): Page titles such as Dashboard, Job Templates, and Strategy Management.
- **Title** (850, 0.98rem, 1.2): Panel titles and modal titles.
- **Body** (400, 14px, 1.42): Default interface text, table content, forms, and descriptions.
- **Label** (700-850, 0.76rem-0.82rem, uppercase only for metric labels): Form labels, legends, metric labels, status chips, and compact metadata.
- **Mono** (0.78rem-0.82rem, 1.45-1.5): Code fields, logs, template JSON, IDs, and query previews.

### Named Rules

**The No Hero Type Rule.** This is a control UI. Do not introduce oversized display type, fluid headings, or marketing-style hierarchy.

**The Data Weight Rule.** Use heavier weights for labels and values only when they improve scanning. Do not bold entire blocks of explanatory text.

## 4. Elevation

The system uses light structural depth everywhere: borders and tonal fills do most of the work, with subtle shadows on panels and heavier elevation reserved for dialogs. Shadows should be quiet enough that layout structure remains the dominant cue.

### Shadow Vocabulary
- **Panel Hairline Lift** (`0 1px 2px rgba(15, 23, 42, 0.025)`): Default panels, stat cells, and compact items.
- **Segment Lift** (`0 1px 2px rgba(15, 23, 42, 0.05)`): Selected export tabs and small raised controls.
- **Modal Lift** (`0 24px 70px rgba(15, 23, 42, 0.24)`): Dialogs only.
- **Status Halo** (`0 0 0 3px rgba(148, 163, 184, 0.16)` or `rgba(22, 163, 74, 0.16)`): Tiny live status indicators only.

### Named Rules

**The Modal Owns Height Rule.** Heavy shadow belongs to modals. Panels stay lightly lifted and structurally bordered.

**The Border First Rule.** If depth is needed, start with border and background contrast before adding stronger shadow.

## 5. Components

### Buttons

Compact, direct, and stateful. Buttons should look like tools, not calls to action from a landing page.

- **Shape:** Gently curved controls (6px radius), with icon buttons fixed at 32px square.
- **Primary:** Operator Blue background, white text, 38px minimum height for full-width form submits.
- **Hover / Focus:** Hover shifts border and background within the same blue family. Focus uses a 3px pale blue outline.
- **Secondary / Ghost / Tertiary:** White or transparent surface, cool border, slate text, and blue hover text. Secondary buttons rely on weight, not color fill.
- **Danger:** Failure Red text by default. Use filled danger only for status badges, not routine destructive buttons unless confirmation is present.

### Chips

Small state labels and metadata pills keep tables readable without adding platform chrome.

- **Style:** 5px radius for status chips, pill radius for requirement badges, tight padding, heavy label weight.
- **State:** Status chips carry semantic background, text, and border. Metadata chips use neutral fill and subdued labels.

### Cards / Containers

Panels are working surfaces with compact headers and clear separation.

- **Corner Style:** 8px panel radius.
- **Background:** Surface White on Workspace Mist.
- **Shadow Strategy:** Panel Hairline Lift plus cool border.
- **Border:** Always present on panels and compact items.
- **Internal Padding:** 12-14px for panel headers and bodies; 9-11px for compact repeated items.

### Inputs / Fields

Inputs should feel exact and stable, with enough padding for mobile use.

- **Style:** White surface, Strong Cool Border, 6px radius, 8px 9px padding.
- **Focus:** Shared 3px pale blue focus outline. Do not replace with subtle border-only focus.
- **Error / Disabled:** Error copy uses Failure Red. Disabled controls use subdued slate text on pale neutral fill.

### Navigation

Top navigation stays plain and readable.

- **Style:** Sticky white topbar with cool border and compact grid layout.
- **Typography:** Heavy 750 navigation labels with muted inactive color.
- **Default / Hover / Active:** Inactive links are muted. Hover uses pale neutral fill. Active uses Operator Blue fill with white text.
- **Mobile Treatment:** Topbar collapses to one column under 1100px; nav scrolls horizontally if needed.

### Tables and Lists

Tables carry the highest information density. Lists carry saved templates, strategies, and selected items.

- **Table:** Minimum wide table with horizontal overflow, fixed columns, uppercase header labels, and hover row tint.
- **Progress:** 6px pill track with Success Green fill and compact supporting stats.
- **Compact List:** 8px panels with 9px gaps. Selected state uses Soft Operator Blue and stronger cool border.

### Dialogs

Dialogs are the only strongly elevated surface.

- **Shape:** 8px radius with cool border.
- **Layout:** Header, scrollable body, action footer. Keep body grid responsive at 640px.
- **Backdrop:** Dark slate overlay. Do not use blur or glass effects.

## 6. Do's and Don'ts

### Do:

- **Do** keep the Web UI dense but calm: compact spacing, clear borders, and predictable grids.
- **Do** use Operator Blue only for navigation selection, primary actions, active filters, and pending or paused states.
- **Do** keep status colors tied to real states: running, done, blocked, failed, warning, required, optional.
- **Do** preserve mobile viewing with structural collapse at 1100px and 640px.
- **Do** use monospace only for logs, JSON, IDs, code, and query previews.
- **Do** keep focus visible with the shared 3px pale blue outline.

### Don't:

- **Don't** make the UI feel like a complicated platform.
- **Don't** make it feel like enterprise BI.
- **Don't** use hacker-terminal styling as the main surface.
- **Don't** create generic admin-template sprawl with repeated decorative cards.
- **Don't** add marketing hero typography, hero metrics, gradient text, decorative gradients, or glassmorphism.
- **Don't** use semantic colors for decoration.
- **Don't** add heavy shadows to panels; reserve heavy elevation for dialogs.
