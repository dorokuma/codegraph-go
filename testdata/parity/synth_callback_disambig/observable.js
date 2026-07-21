// Field-backed observer shared by two classes.
// Each class defines a handle() method; fieldChannelEdges must
// disambiguate by registration context (same-file preference),
// not blindly pick the first global "handle" Method.
export class Observable {
  constructor() {
    this.callbacks = new Set();
  }
  onUpdate(cb) {
    this.callbacks.add(cb);
  }
  triggerUpdate() {
    for (const cb of this.callbacks) {
      cb();
    }
  }
}
