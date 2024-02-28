package com.example.customer.pact.consumer;

import au.com.dius.pact.consumer.Pact;
import au.com.dius.pact.consumer.PactProviderRuleMk2;
import au.com.dius.pact.consumer.PactVerification;
import au.com.dius.pact.consumer.dsl.PactDslJsonBody;
import au.com.dius.pact.consumer.dsl.PactDslWithProvider;
import au.com.dius.pact.model.RequestResponsePact;
import com.example.customer.core.CustomerService;
import com.example.customer.core.Order;
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
  private CustomerService customerService;

  @Rule
  public PactProviderRuleMk2 mockProvider = new PactProviderRuleMk2("retailer",
      HOST_NAME, PORT, this);

  @Pact(consumer = "customer", provider = "retailer")
  public RequestResponsePact createPactForGetLastUpdatedTimestamp(PactDslWithProvider builder) {
    PactDslJsonBody body = new PactDslJsonBody()
            .stringType("customer", "John")
            .decimalType("total", 2000.0)
            .integerType("noOfItems", 3);

    Map<String,String> headers = new HashMap();
    headers.put("Content-Type","application/json");

    return builder
        .given("Get order details")
        .uponReceiving("Get order details by order id")
        .path("/order/79")
        .method(HttpMethod.GET.name())
        .willRespondWith()
        .status(HttpStatus.OK.value())
        .headers(headers)
        .body(body)
        .toPact();
  }

  @Test
  @PactVerification(value = "retailer")
  public void testGetOrderFromRetailer() {
    Order order = customerService.getOrderDetails();
    assertEquals(order.getCustomer(), "John");
    assertEquals(order.getTotal(), 2000.0, 1);
    assertEquals(order.getNoOfItems().intValue(), 3);
  }
}
