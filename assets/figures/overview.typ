// Compact managed flow; command details live in the project wiki.
#let theme = sys.inputs.at("theme", default: "light")
#assert(("light", "dark").contains(theme), message: "Unknown theme")
#let dark = theme == "dark"
#let ink = rgb(if dark { "f0f0e9" } else { "20282d" })
#let paper = rgb(if dark { "121920" } else { "f6f5f1" })
#let panel = rgb(if dark { "1b242b" } else { "fffefa" })
#let plane = rgb(if dark { "202d34" } else { "eceeea" })
#let border = rgb(if dark { "3e4b52" } else { "d6dcd7" })
#let gold = rgb(if dark { "d9b574" } else { "a57a35" })
#let gold-text = rgb(if dark { "d9b574" } else { "805c24" })
#let tint = rgb(if dark { "332e22" } else { "f2e8d4" })

#set page(width: 960pt, height: 390pt, margin: 0pt, fill: paper)
#set text(font: "FreeSans", fill: ink, top-edge: "cap-height", bottom-edge: "descender")
#let put(x, y, body) = place(top + left, dx: x * 1pt, dy: y * 1pt, body)
#let words(x, y, body, size: 18, fill: ink, weight: "bold") = put(x, y, text(size: size * 1pt, fill: fill, weight: weight, body))
#let rect(x, y, w, h, fill: panel, radius: 10) = put(x, y,
  box(width: w * 1pt, height: h * 1pt, fill: fill, stroke: border + 0.8pt, radius: radius * 1pt))
#let command(x, y, value) = put(x, y, box(fill: tint, radius: 4pt, inset: (x: 8pt, y: 7pt),
  text(font: "TeX Gyre Cursor", size: 12.5pt, weight: "bold", fill: gold-text, value)))
#let number(x, y, value) = put(x, y, circle(width: 24pt, fill: tint, stroke: none, inset: 0pt,
  align(center + horizon, text(size: 11pt, weight: "bold", fill: gold-text, value))))
#let arrow(x1, x2, y, returning: false) = {
  let color = if returning { gold } else { ink }
  put(calc.min(x1, x2), y, line(length: calc.abs(x2 - x1) * 1pt,
    stroke: (paint: color, thickness: 1.5pt, dash: if returning { "dashed" } else { "solid" })))
  if x2 > x1 {
    put(x2 - 7, y - 4, polygon((0pt, 0pt), (7pt, 4pt), (0pt, 8pt), fill: color, stroke: none))
  } else {
    put(x2, y - 4, polygon((7pt, 0pt), (0pt, 4pt), (7pt, 8pt), fill: color, stroke: none))
  }
}

#rect(20, 20, 574, 350, fill: plane, radius: 14)
#rect(634, 20, 306, 350, fill: plane, radius: 14)
#words(40, 39, "CONTROLLER", size: 12)
#words(654, 39, "WORKERS", size: 12)

#rect(40, 70, 190, 148)
#number(54, 88, "01")
#words(88, 91, "Submit")
#command(56, 130, "ordo submit")
#arrow(230, 270, 150)

#rect(270, 70, 304, 276)
#number(286, 88, "02")
#words(320, 91, "Schedule")
#command(290, 130, "ordo server")
#words(290, 184, "Durable queue", size: 14)
#for i in range(4) {
  rect(290 + i * 42, 210, 32, 15, fill: if i == 0 { tint } else { plane }, radius: 3)
}
#put(290, 255, line(length: 264pt, stroke: border + 0.8pt))
#number(290, 284, "04")
#words(324, 287, "Collect & verify", size: 16)

#rect(40, 250, 190, 96)
#number(54, 266, "05")
#words(88, 269, "Deliver")
#command(56, 309, "ordo submit --wait")
#arrow(270, 230, 300, returning: true)

#rect(654, 70, 266, 276)
#number(670, 88, "03")
#words(704, 91, "Execute")
#command(670, 130, "podman run")
#put(787, 174, line(length: 40pt, angle: 90deg, stroke: ink + 1.5pt))
#put(783, 207, polygon((0pt, 0pt), (8pt, 0pt), (4pt, 7pt), fill: ink, stroke: none))
#rect(670, 220, 234, 106, fill: plane, radius: 7)
#words(686, 239, "Rootless Podman", size: 16)
#for (i, label) in ("cores", "GPUs", "RAM").enumerate() {
  rect(686 + i * 67, 279, 54, 25, fill: panel, radius: 4)
  words(696 + i * 67, 287, label, size: 11)
}

#arrow(574, 654, 150)
#words(599, 124, "SSH", size: 12)
#arrow(654, 574, 300, returning: true)
#words(592, 320, "results", size: 11, fill: gold-text, weight: "regular")
