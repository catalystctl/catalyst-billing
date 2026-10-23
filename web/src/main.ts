// Entry point.
//
// Two things happen before the app renders: the theme is applied (so the first paint is the
// right colour) and the stylesheet import is pulled in by the bundler. Then the app takes
// over the #root element that the embedded shell provides.

import "./styles/tokens.css";
import "./styles/base.css";
import "./styles/layout.css";
import "./styles/components.css";
import "./styles/tables.css";
import "./styles/pages.css";
import "./styles/view-customers.css";
import "./styles/view-invoices.css";
import "./styles/view-services.css";
import "./styles/view-queue.css";
import "./styles/view-money.css";
import "./styles/view-trust.css";
import "./styles/view-portal.css";
import "./styles/view-storefront.css";
import "./styles/view-catalog.css";
import "./styles/view-commerce.css";
import "./styles/view-support.css";
import "./styles/view-platform.css";
import "./styles/view-branding.css";

import { bootstrap } from "./app";

const root = document.getElementById("root");
if (!root) throw new Error("the shell is missing #root");

void bootstrap(root);
