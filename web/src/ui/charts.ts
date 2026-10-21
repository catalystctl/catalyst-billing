// Small charts, drawn as SVG.
//
// A sparkline earns its place because it answers a question a number cannot: is this
// trending, or is it one bad day? No charting library is worth 80KB for two shapes, and a
// library would also need its own CSP allowances for injected styles.

const NS = "http://www.w3.org/2000/svg";

export function sparkline(values: number[], opts: { width?: number; height?: number; tone?: string } = {}): SVGElement {
  const width = opts.width ?? 220;
  const height = opts.height ?? 30;
  const pad = 2;
  const svg = document.createElementNS(NS, "svg");
  svg.setAttribute("viewBox", `0 0 ${width} ${height}`);
  svg.setAttribute("preserveAspectRatio", "none");
  svg.setAttribute("class", "stat-spark");
  svg.setAttribute("aria-hidden", "true");

  if (values.length < 2) return svg;

  const max = Math.max(...values);
  const min = Math.min(...values);
  const span = max - min || 1;
  const step = (width - pad * 2) / (values.length - 1);
  const points = values.map((value, i) => {
    const x = pad + i * step;
    const y = height - pad - ((value - min) / span) * (height - pad * 2);
    return [x, y] as const;
  });

  const line = points.map(([x, y], i) => `${i === 0 ? "M" : "L"}${x.toFixed(1)} ${y.toFixed(1)}`).join(" ");
  const area = `${line} L${(width - pad).toFixed(1)} ${height} L${pad} ${height} Z`;

  const gradientId = `spark-${Math.random().toString(36).slice(2, 8)}`;
  const defs = document.createElementNS(NS, "defs");
  const gradient = document.createElementNS(NS, "linearGradient");
  gradient.setAttribute("id", gradientId);
  gradient.setAttribute("x1", "0");
  gradient.setAttribute("y1", "0");
  gradient.setAttribute("x2", "0");
  gradient.setAttribute("y2", "1");
  for (const [offset, opacity] of [
    ["0", "0.28"],
    ["1", "0"],
  ] as const) {
    const stop = document.createElementNS(NS, "stop");
    stop.setAttribute("offset", offset);
    stop.setAttribute("stop-color", "currentColor");
    stop.setAttribute("stop-opacity", opacity);
    gradient.appendChild(stop);
  }
  defs.appendChild(gradient);
  svg.appendChild(defs);

  const areaPath = document.createElementNS(NS, "path");
  areaPath.setAttribute("d", area);
  areaPath.setAttribute("fill", `url(#${gradientId})`);
  svg.appendChild(areaPath);

  const linePath = document.createElementNS(NS, "path");
  linePath.setAttribute("d", line);
  linePath.setAttribute("fill", "none");
  linePath.setAttribute("stroke", "currentColor");
  linePath.setAttribute("stroke-width", "1.6");
  linePath.setAttribute("stroke-linecap", "round");
  linePath.setAttribute("stroke-linejoin", "round");
  linePath.setAttribute("vector-effect", "non-scaling-stroke");
  svg.appendChild(linePath);

  return svg;
}
