import { h } from "../lib/dom";
import type { Ctx } from "../lib/types";

export const meta = { title: "Services" };

export default async function page(_ctx: Ctx): Promise<Node> {
  return h("div", { class: "page" }, h("h1", { class: "page-title" }, "Services"));
}
