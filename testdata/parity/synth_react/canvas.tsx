// React class re-render + JSX child (both required end-to-end).
export class Canvas {
  dirty() {
    this.setState({ n: 1 });
  }
  noop() {
    return 0;
  }
  render() {
    return <StaticCanvas />;
  }
}
