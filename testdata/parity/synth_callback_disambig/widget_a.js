import { Observable } from "./observable.js";

export class WidgetA {
  constructor() {
    this.bus = new Observable();
    this.bus.onUpdate(this.handle);
  }
  handle() {
    this.doA();
  }
  doA() {}
  fire() {
    this.bus.triggerUpdate();
  }
}
