/* An adaptive SNR-threshold noise gate. It does not enhance the physical SNR. */
class SNRGate extends AudioWorkletProcessor {
  constructor() {
    super();
    this.floor = 0.0018;
    this.envelope = 0;
    this.open = false;
    this.thresholdDb = 6;
    this.port.onmessage = (event) => {
      if (Number.isFinite(event.data?.thresholdDb)) this.thresholdDb = Math.max(0, Math.min(24, event.data.thresholdDb));
    };
  }
  process(inputs, outputs) {
    const input = inputs[0], output = outputs[0];
    if (!output?.length) return true;
    const source = input?.[0];
    if (!source) { output[0].fill(0); return true; }
    let power = 0;
    for (let i = 0; i < source.length; i++) power += source[i] * source[i];
    const rms = Math.sqrt(power / source.length);
    // Estimate background level from quiet frames, and do not let speech rapidly raise it.
    if (rms < this.floor * 2.0 || rms < 0.004) {
      this.floor = Math.max(0.00015, Math.min(0.08, this.floor * 0.996 + rms * 0.004));
    }
    const threshold = this.floor * Math.pow(10, this.thresholdDb / 20);
    const wantsOpen = this.thresholdDb === 0 ? true : rms > threshold * (this.open ? 0.72 : 1.0);
    this.open = wantsOpen;
    const aim = wantsOpen ? 1 : 0;
    for (let c = 0; c < output.length; c++) {
      const out = output[c], src = input[c] || source;
      for (let i = 0; i < out.length; i++) {
        this.envelope += (aim - this.envelope) * (aim ? 0.007 : 0.0015);
        out[i] = src[i] * this.envelope;
      }
    }
    return true;
  }
}
registerProcessor('snr-gate', SNRGate);
