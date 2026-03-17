/**
 * EventFeed — vertical ticker tape on the right edge of the pipeline canvas.
 *
 * Spec (§3.2):
 *   - Up to 50 rows, newest at top, fades out at bottom
 *   - Each row: [timestamp] [event_type] [source→target]  monospace, colour-coded by event type
 *   - Right-edge overlay, not a separate panel
 */
import { memo } from 'react';
import type { WebEvent } from '../../types/protocol';
import { EVENT_LABELS, particleColor } from '../../types/protocol';
import styles from './EventFeed.module.css';

const MAX_ROWS = 50;

interface EventFeedProps {
  events: WebEvent[];
}

function EventFeed({ events }: EventFeedProps) {
  const visible = events.slice(0, MAX_ROWS);

  return (
    <div className={styles.feed} aria-label="Event feed" role="log" aria-live="polite">
      {visible.map((ev, i) => {
        const color = particleColor(ev.type);
        const label = EVENT_LABELS[ev.type] ?? ev.type;
        const ts = formatTs(ev.timestamp);
        const opacity = Math.max(0.15, 1 - i / MAX_ROWS);

        return (
          <div
            key={`${ev.timestamp}-${ev.type}-${ev.source}-${ev.target}`}
            className={styles.row}
            style={{ opacity }}
          >
            <span className={styles.ts}>{ts}</span>
            <span className={styles.evType} style={{ color }}>{label}</span>
            <span className={styles.route}>{ev.source}→{ev.target}</span>
          </div>
        );
      })}
    </div>
  );
}

function formatTs(iso: string): string {
  try {
    return new Date(iso).toLocaleTimeString('en-GB', {
      hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false,
    });
  } catch {
    return '--:--:--';
  }
}

export default memo(EventFeed);
