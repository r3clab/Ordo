// Keep the O as editable SVG; Typst outlines the FreeSerif lettering.
#let theme = sys.inputs.at("theme", default: "light")
#let mode = sys.inputs.at("mode", default: "wordmark")
#assert(("light", "dark").contains(theme), message: "Unknown theme")
#assert(("wordmark", "icon").contains(mode), message: "Unknown output mode")
#let dark = theme == "dark"
#let ink = if dark { rgb("f0f0e9") } else { rgb("20282d") }
#let gold = if dark { "#d9b574" } else { "#a57a35" }
#let icon = read("folio-icon.svg").replace("#20282d", ink.to-hex()).replace("#a57a35", gold)

#set page(margin: 0pt, fill: none)
#set text(font: "FreeSerif", fill: ink, size: 124pt, tracking: -1pt,
  top-edge: "cap-height", bottom-edge: "baseline")
#set par(leading: 0pt)

#if mode == "icon" {
  set page(width: 96pt, height: 96pt)
  image(bytes(icon), width: 96pt)
} else {
  set page(width: 332pt, height: 140pt)
  place(top + left, dx: 38pt, dy: 22pt, image(bytes(icon), width: 96pt))
  place(top + left, dx: 122pt, dy: 28pt)[rdo]
}
