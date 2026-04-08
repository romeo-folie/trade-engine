/**
 * ErrorBoundary — catches render-time exceptions in its subtree and shows
 * a small fallback UI instead of unmounting the entire React tree.
 *
 * Without this, a single TypeError in a node card (e.g. `.toFixed` on
 * undefined) would wipe the whole pipeline graph and force a page refresh.
 */
import { Component, type ReactNode, type ErrorInfo } from 'react';

interface Props {
  children: ReactNode;
  fallback?: ReactNode;
  onReset?: () => void;
}

interface State {
  error: Error | null;
}

export default class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    // Log loudly so the offending stack is visible in devtools.
    console.error('[ErrorBoundary] caught render error:', error, info.componentStack);
  }

  handleReset = () => {
    this.setState({ error: null });
    this.props.onReset?.();
  };

  render() {
    if (this.state.error) {
      if (this.props.fallback) return this.props.fallback;
      return (
        <div
          style={{
            padding: 16,
            margin: 16,
            border: '1px solid #b00020',
            borderRadius: 6,
            background: '#1a0808',
            color: '#ffb3b3',
            fontFamily: 'var(--font-mono, monospace)',
            fontSize: 12,
          }}
        >
          <div style={{ fontWeight: 700, marginBottom: 8 }}>
            Pipeline render error
          </div>
          <div style={{ marginBottom: 8, opacity: 0.85 }}>
            {this.state.error.message}
          </div>
          <button
            onClick={this.handleReset}
            style={{
              padding: '4px 10px',
              background: '#b00020',
              color: '#fff',
              border: 'none',
              borderRadius: 4,
              cursor: 'pointer',
            }}
          >
            Retry
          </button>
        </div>
      );
    }
    return this.props.children;
  }
}
