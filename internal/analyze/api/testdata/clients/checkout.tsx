// A React component of the shape this feature exists for: the calls here are
// the missing hop between a UI symbol and the backend endpoint it depends on.
import axios from "axios";

const BASE = "/api";

export function useTransactions() {
  const load = async () => {
    const res = await fetch("/api/transactions");
    return res.json();
  };
  return load;
}

export async function submitPayment(body: PaymentRequest) {
  return fetch("/payment/callback", {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });
}

export async function refundPayment(id: string) {
  return axios.post(`/payment/refund/${id}`, {});
}

export async function fetchTransaction(id: string) {
  // A template literal: the prefix is knowable, the rest is not.
  const res = await fetch(`/api/transactions/${id}`);
  return res.json();
}

export async function search(query: string) {
  // A concatenation, which is the same problem written differently.
  return axios.get("/api/search?q=" + encodeURIComponent(query));
}

export async function callAnything(url: string) {
  // Nothing literal at all. Still a real outbound call.
  return fetch(url);
}

export class TransactionApi {
  async remove(id: string) {
    return axios.delete("/api/transactions/" + id);
  }
}
