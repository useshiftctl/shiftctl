# Design fidelity rules — apply to every screen build

## Source of truth
- Build only from the **Hi-Fi page** in Figma, never the wireframes page. They live in the same file on separate pages — confirm which page a frame is on before using it.
- Read the actual Figma frame directly via the Figma MCP tools for the specific screen being built, every time. Never build from a written summary, a past conversation's description, or memory of a similar screen.

## Signs you're looking at the wrong source (stop and re-check if you see these)
- Grayscale only, no color
- A visible "MAIN PANEL" or "SIDE PANEL" label — these were removed from every hi-fi screen and never existed on final versions
- Plain HTML-default buttons/inputs instead of the Rayna UI component styling (rounded pills for badges, specific button shapes)
- Generic system font instead of Inter

## What to match exactly, pulled from the Figma node, not approximated
- **Color:** exact hex/RGB values from the frame's fills, including the Dodger Blue brand mode already applied — never a close guess
- **Typography:** exact font size, weight, and line height from the actual text node — Heading/H5/Bold for screen titles, Paragraph/md Semi Bold for card titles, Paragraph/xs/Regular for body text
- **Spacing:** exact padding and gap values from the frame's auto-layout properties — not eyeballed
- **Copy:** exact text content, character for character, including all hyphens/punctuation as written

## After building each screen
- Take a screenshot of the built screen and the Figma frame side by side
- Flag any difference you notice yourself before showing it to me — spacing, color, missing states, wrong copy
- If a screen has multiple states (Assessment, Approval, Staged Execution, Decommission all do), build and show every state, not just the default one
