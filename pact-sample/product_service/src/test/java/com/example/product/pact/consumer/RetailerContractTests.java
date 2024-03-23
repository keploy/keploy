package com.example.product.pact.consumer;

import au.com.dius.pact.consumer.Pact;
import au.com.dius.pact.consumer.PactProviderRuleMk2;
import au.com.dius.pact.consumer.PactVerification;
import au.com.dius.pact.consumer.dsl.PactDslJsonBody;
import au.com.dius.pact.consumer.dsl.PactDslWithProvider;
import au.com.dius.pact.model.RequestResponsePact;
import com.example.product.core.ProductService;
import com.example.product.core.Item;
import org.junit.Rule;
import org.junit.Test;
import org.junit.runner.RunWith;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.http.HttpMethod;
import org.springframework.http.HttpStatus;
import org.springframework.test.context.junit4.SpringRunner;

import java.util.HashMap;
import java.util.Map;

import static org.junit.Assert.assertEquals;

@RunWith(SpringRunner.class)
@SpringBootTest
public class RetailerContractTests {

  private static final String HOST_NAME = "localhost";
  private static final int PORT = 8088;

  @Autowired
  private ProductService productService;

  @Rule
  public PactProviderRuleMk2 mockProvider = new PactProviderRuleMk2("retailer",
      HOST_NAME, PORT, this);

  @Pact(consumer = "product", provider = "retailer")
  public RequestResponsePact createPactForGetLastUpdatedTimestamp(PactDslWithProvider builder) {

    PactDslJsonBody body = new PactDslJsonBody()
            .stringType("brand", "Apple")
            .stringType("name", "iPhone")
            .decimalType("price", 1000.0);

    Map<String,String> headers = new HashMap();
    headers.put("Content-Type","application/json");

    return builder
        .given("Get item details")
        .uponReceiving("Get item details for item id")
        .path("/item/1009")
        .method(HttpMethod.GET.name())
        .willRespondWith()
        .status(HttpStatus.OK.value())
        .headers(headers)
        .body(body)
        .toPact();
  }

  @Test
  @PactVerification(value = "retailer")
  public void testGetItemDetailsFromRetailer() {
    Item item = productService.getItemDetail();
    assertEquals(item.getName(), "iPhone");
  }

}
