import { Component, ReactNode } from 'react';

interface Props {
  children: ReactNode;
  fallback?: ReactNode;
}

interface State {
  hasError: boolean;
  error?: Error;
}

/**
 * Generic ErrorBoundary that catches rendering errors and shows a fallback UI
 * instead of crashing the whole page. Use this to wrap chart components that
 * may receive malformed data from broker API fallbacks.
 */
export default class ErrorBoundary extends Component<Props, State> {
  constructor(props: Props) {
    super(props);
    this.state = { hasError: false };
  }

  static getDerivedStateFromError(error: Error): State {
    return { hasError: true, error };
  }

  componentDidCatch(error: Error) {
    console.error('[ErrorBoundary] caught error:', error);
  }

  render() {
    if (this.state.hasError) {
      return this.props.fallback ?? (
        <div style={{
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
          height: '100%',
          color: '#6B7280',
          fontFamily: '"JetBrains Mono", monospace',
          fontSize: 12,
        }}>
          Chart unavailable
        </div>
      );
    }
    return this.props.children;
  }
}
