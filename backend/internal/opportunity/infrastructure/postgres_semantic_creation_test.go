package infrastructure

import (
	"context"
	"testing"
	"time"

	"lidradar/backend/internal/opportunity/domain"
	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/ids"
)

func TestSemanticCreationUnknownPriceFreshnessClosureAndIsolation(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	conversation := insertOpportunityConversation(t, pool, pair.A.TenantID, pair.A.LocationID)
	id := func() string {
		v, e := (ids.Generator{}).NewID()
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, e := pool.Exec(ctx, query, args...); e != nil {
			t.Fatal(e)
		}
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	message, node, job, run := id(), id(), id(), id()
	exec(`INSERT INTO messages(id,tenant_id,conversation_id,connection_id,external_id,direction,type,text,sent_at,received_at,metadata,created_at)
 SELECT $1::uuid,tenant_id,id,connection_id,$1::text,'INCOMING','TEXT','Хочу записаться на услугу',$3,$3,'{}',$3 FROM conversations WHERE id=$2`, message, conversation, at)
	exec(`UPDATE conversations SET revision=1 WHERE id=$1`, conversation)
	exec(`INSERT INTO ai_nodes(id,name,secret_digest,status,created_at,updated_at) VALUES($1::uuid,$1::text,decode(repeat('00',32),'hex'),'OFFLINE',$2,$2)`, node, at)
	exec(`INSERT INTO ai_node_tenants(node_id,tenant_id,created_at) VALUES($1,$2,$3)`, node, pair.A.TenantID, at)
	exec(`INSERT INTO ai_jobs(id,tenant_id,job_type,entity_type,entity_id,payload,model_requirement,schema_version,prompt_version,base_conversation_revision,analysis_through_message_id,status,available_at,created_at,updated_at)
 VALUES($1,$2,'ANALYZE_CONVERSATION','CONVERSATION',$3,'{}','model','analyze-conversation.v2','v7',1,$4,'PENDING',$5,$5,$5)`, job, pair.A.TenantID, conversation, message, at)
	exec(`INSERT INTO ai_runs(id,tenant_id,job_id,node_id,entity_type,entity_id,status,application_status,base_conversation_revision,analysis_through_message_id,model_version,prompt_version,schema_version,raw_output,started_at,completed_at)
 VALUES($1,$2,$3,$4,'CONVERSATION',$5,'SUCCEEDED','APPLIED',1,$6,'model','v7','analyze-conversation.v2','{}',$7,$7)`, run, pair.A.TenantID, job, node, conversation, message, at)
	exec(`INSERT INTO conversation_summaries(tenant_id,conversation_id,summary_text,base_conversation_revision,analysis_through_message_id,model_version,prompt_version,schema_version,ai_run_id,updated_at,semantic_facts)
 VALUES($1,$2,'Запрос записи',1,$3::uuid,'model','v7','analyze-conversation.v2',$4,$5,
 jsonb_build_array(jsonb_build_object('type','BOOKING_INTENT','value',true,'confidence',0.95,'trusted',true,'evidenceMessageIds',jsonb_build_array($3::text))))`, pair.A.TenantID, conversation, message, run, at)
	repository := NewPostgresRepository(pool)
	command := domain.SemanticCreation{TenantID: pair.A.TenantID, ConversationID: conversation, RunID: run, ThroughMessageID: message, Revision: 1, OpportunityID: id(), HistoryID: id(), At: at.Add(time.Minute)}
	foreign := command
	foreign.TenantID = pair.B.TenantID
	if _, found, e := repository.CreateFromAnalysis(ctx, foreign); e != nil || found {
		t.Fatalf("foreign: %v %v", found, e)
	}
	stale := command
	stale.Revision = 2
	if _, found, e := repository.CreateFromAnalysis(ctx, stale); e != nil || found {
		t.Fatalf("stale: %v %v", found, e)
	}
	exec(`UPDATE conversation_summaries SET semantic_facts=jsonb_set(semantic_facts,'{0,trusted}','false') WHERE conversation_id=$1`, conversation)
	if _, found, e := repository.CreateFromAnalysis(ctx, command); e != nil || found {
		t.Fatalf("untrusted: %v %v", found, e)
	}
	exec(`UPDATE conversation_summaries SET semantic_facts=jsonb_set(semantic_facts,'{0,trusted}','true') WHERE conversation_id=$1`, conversation)
	created, found, e := repository.CreateFromAnalysis(ctx, command)
	if e != nil || !found || created.ServiceID != nil || created.EstimatedAmount != nil || created.EstimatedAmountConfidence != nil || created.Currency != "RUB" {
		t.Fatalf("unknown creation: %#v %v %v", created, found, e)
	}
	repeated, found, e := repository.CreateFromAnalysis(ctx, command)
	if e != nil || !found || repeated.ID != created.ID {
		t.Fatalf("duplicate: %#v %v %v", repeated, found, e)
	}
	detail, _, e := repository.Detail(ctx, pair.A.TenantID, created.ID)
	if e != nil || len(detail.History) != 1 || detail.History[0].Source != domain.SourceAI {
		t.Fatalf("history: %#v %v", detail, e)
	}
	serviceID := id()
	exec(`INSERT INTO service_catalog_items(id,tenant_id,name,normalized_name,currency) VALUES($1,$2,'Чистка дивана','чистка дивана','RUB')`, serviceID, pair.A.TenantID)
	quoted, _ := domain.ParsePotentialRevenue("2800")
	confidence, _ := domain.ParseConfidence("0.95")
	if _, e = repository.UpdateEstimate(ctx, domain.EstimateUpdate{TenantID: pair.A.TenantID, OpportunityID: created.ID, Amount: quoted, Confidence: confidence, Currency: "RUB", At: at.Add(time.Minute)}); e != nil {
		t.Fatal(e)
	}
	catalogAmount, _ := domain.ParsePotentialRevenue("3500")
	enriched, changed, e := repository.EnrichService(ctx, domain.ServiceEnrichment{TenantID: pair.A.TenantID, OpportunityID: created.ID, ServiceID: serviceID, Amount: &catalogAmount, Confidence: &confidence, Currency: "RUB", At: at.Add(time.Minute)})
	if e != nil || !changed || enriched.ServiceID == nil || *enriched.ServiceID != serviceID || enriched.EstimatedAmount.String() != "2800.00" {
		t.Fatalf("enrichment preserves quote: %#v %v %v", enriched, changed, e)
	}
	_, _, e = repository.Transition(ctx, domain.TransitionCommand{TenantID: pair.A.TenantID, OpportunityID: created.ID, HistoryID: id(), ToStage: domain.StageLost, Source: domain.SourceUser, ActorUserID: &pair.A.UserID, At: at.Add(2 * time.Minute)})
	if e != nil {
		t.Fatal(e)
	}
	command.OpportunityID = id()
	command.HistoryID = id()
	command.At = at.Add(3 * time.Minute)
	if _, found, e := repository.CreateFromAnalysis(ctx, command); e != nil || found {
		t.Fatalf("old intent reopened closed deal: %v %v", found, e)
	}
	// A new commercial intent after closure may start another deal, even without
	// a catalogue match. A removed or edited current message invalidates the snapshot.
	exec(`UPDATE messages SET sent_at=$2 WHERE id=$1`, message, at.Add(150*time.Second))
	exec(`UPDATE conversations SET revision=2 WHERE id=$1`, conversation)
	if _, found, e := repository.CreateFromAnalysis(ctx, command); e != nil || found {
		t.Fatalf("edited snapshot: %v %v", found, e)
	}
	exec(`UPDATE conversation_summaries SET base_conversation_revision=2,semantic_facts=jsonb_set(semantic_facts,'{0,type}','"PURCHASE_INTENT"') WHERE conversation_id=$1`, conversation)
	command.Revision = 2
	if _, found, e := repository.CreateFromAnalysis(ctx, command); e != nil || !found {
		t.Fatalf("new purchase intent: %v %v", found, e)
	}
}
