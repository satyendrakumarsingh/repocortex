package com.marketing;

/** Unrelated to payments. get_task_context must NOT return this. */
public class MarketingReport {
    public void renderCampaignSummary() {
        analytics.aggregate("campaign_clicks");
    }

    public void exportToCsv(String path) {
        writer.write(path);
    }
}
