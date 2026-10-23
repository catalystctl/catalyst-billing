// The announcement feed, as a customer sees it.
//
// One module for two surfaces — the storefront and the customer dashboard — because both are
// governed by the same two rules and a rule written twice is a rule that drifts:
//
//   - **The server decides what is live.** `live` and `state` are derived from the server's clock
//     (`announcement.state` in internal/httpapi/announcements.go), not from the browser's. A
//     screen that re-derived the answer would disagree with the feed a customer's request gets
//     whenever the two clocks differ, and the staff screen would be the one that lied.
//   - **The setting decides whether there is a feed at all.** `announcements.enabled` is answered
//     by the same request as `enabled`. When it is false this module renders *nothing* — no card,
//     no heading, no empty state. An empty card would say "there are no announcements" when the
//     truth is "the operator switched the feed off", which is exactly the kind of quiet lie this
//     feature exists to stop telling.
//
// A failed fetch renders nothing too: a notice feed is decoration, and a portal that shows an
// error banner because a maintenance notice did not load is worse than a portal without the
// notice.

import type { Api } from "../lib/api";
import { h } from "../lib/dom";
import { relative } from "../lib/format";
import { tx } from "../lib/i18n";

/** One announcement, verbatim from the `announcement` struct in
 *  internal/httpapi/announcements.go. `state` is one of draft, scheduled, expired or live. */
export interface Announcement {
  id: string;
  title: string;
  body: string;
  placement: string;
  published: boolean;
  published_at: string | null;
  expires_at: string | null;
  live: boolean;
  state: string;
}

/** The feed envelope. `enabled` is the `announcements.enabled` setting as the server read it. */
export interface AnnouncementFeed {
  data?: Announcement[];
  enabled?: boolean;
}

/** The two placements a customer-facing surface may ask for. `both` matches either. */
export type FeedPlacement = "storefront" | "dashboard";

/** Loads the feed for one placement, or nothing at all when the operator switched it off.
 *
 *  The `enabled === false` check is deliberately not `!enabled`: a server that does not send the
 *  field (an older build) still sends rows, and the rows are the stronger signal. Only an
 *  explicit "off" hides the feed. */
export async function loadAnnouncementFeed(
  api: Api,
  placement: FeedPlacement,
  signal?: AbortSignal,
): Promise<Announcement[]> {
  const feed = await api.get<AnnouncementFeed>("/api/v1/announcements", {
    query: { placement },
    cache: false,
    signal,
  });
  if (feed?.enabled === false) return [];
  const rows = Array.isArray(feed?.data) ? feed.data : [];
  // The public feed only returns live rows; the filter is the second half of the same promise,
  // so a server that ever widened the predicate cannot leak a draft into a customer's page.
  return rows.filter((row) => row.live !== false);
}

/** The notices, as cards. Returns null when there is nothing to show.
 *
 *  It reuses the staff screen's card classes on purpose: a notice should look to the person
 *  reading it like it looked to the person who published it, and a second visual language for
 *  the same four columns is how the two screens start disagreeing about what an announcement is.
 *  What it does not reuse is the footer — publish, edit and delete are operator affordances and
 *  have no place in a customer's page. */
export function announcementCards(rows: Announcement[]): HTMLElement | null {
  if (rows.length === 0) return null;
  return h("div", { class: "ann-list" }, ...rows.map(announcementCard));
}

/** The customer dashboard's feed: a heading and the notices, or nothing. */
export function dashboardAnnouncements(rows: Announcement[]): HTMLElement | null {
  const cards = announcementCards(rows);
  if (cards === null) return null;
  return h(
    "section",
    { class: "stack" },
    h("h2", { class: "ann-feed-title" }, tx("announcements.feed.title", "Announcements")),
    cards,
  );
}

/** The storefront's feed: the same notices in the storefront's own section rhythm.
 *
 *  The heading text is the storefront's, not the dashboard's: a visitor has no account yet, so
 *  "Announcements" alone is less useful than saying what kind of thing this is. */
export function storefrontAnnouncements(rows: Announcement[]): HTMLElement | null {
  const cards = announcementCards(rows);
  if (cards === null) return null;
  return h(
    "section",
    { class: "store-section" },
    h(
      "div",
      { class: "store-section-head" },
      h("h2", null, tx("announcements.feed.storefront_title", "Announcements")),
      h("p", { class: "dim" }, tx("announcements.feed.storefront_sub", "Notices from the team, in date order.")),
    ),
    cards,
  );
}

/** One notice. No actions, no state badge: every row here is live, so a "Live" badge would be a
 *  label that never changes. The expiry is shown when there is one, because "until Sunday" is
 *  the part of a maintenance notice a reader plans around. */
function announcementCard(row: Announcement): HTMLElement {
  const body = row.body.trim();
  return h(
    "article",
    { class: "card ann-card ann-card--live" },
    h(
      "header",
      { class: "ann-head" },
      h(
        "div",
        { class: "ann-head-text" },
        h(
          "div",
          { class: "ann-title-row" },
          h("h3", { class: "ann-title" }, row.title || tx("announcements.feed.untitled", "Announcement")),
        ),
        row.expires_at
          ? h(
              "div",
              { class: "ann-meta" },
              h(
                "span",
                null,
                tx("announcements.feed.expires", "until {{when}}", { when: relative(row.expires_at) }),
              ),
            )
          : null,
      ),
    ),
    body
      ? h(
          "div",
          { class: "ann-body" },
          ...body.split(/\n{2,}/).map((para) => h("p", null, para)),
        )
      : null,
  );
}
